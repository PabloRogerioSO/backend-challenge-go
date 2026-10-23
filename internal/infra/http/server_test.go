package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

const (
	testWalletID  = "11111111-1111-4111-8111-111111111111"
	unknownID     = "99999999-9999-4999-8999-999999999999"
	ownTxID       = "22222222-2222-4222-8222-222222222222"
	otherTxID     = "33333333-3333-4333-8333-333333333333"
	testEntryID   = "44444444-4444-4444-8444-444444444444"
	headerIdemKey = "Idempotency-Key"
)

func brl(cents int64) money.Money {
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		panic(err)
	}
	return m
}

// ---------------------------------------------------------------- fake authentication

type principal struct {
	provider string
	scopes   []string
}

type principalKey struct{}

var testTokens = map[string]principal{
	"token-provider-a":  {provider: "provider-a", scopes: []string{"wager.write"}},
	"token-provider-b":  {provider: "provider-b", scopes: []string{"wager.write"}},
	"token-internal":    {provider: "", scopes: []string{"internal.wallet"}},
	"token-no-provider": {provider: "", scopes: []string{"wager.write"}},
}

func fakeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		p, known := testTokens[token]
		if !ok || !known {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func fakeScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := r.Context().Value(principalKey{}).(principal)
			for _, s := range p.scopes {
				if s == scope {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeAPIError(w, http.StatusForbidden, "forbidden", "missing scope "+scope)
		})
	}
}

func fakeProviderID(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p.provider
}

// ---------------------------------------------------------------- fakes

type fakeOpener struct {
	mu      sync.Mutex
	players map[string]bool
	seq     int
}

func (f *fakeOpener) OpenWallet(_ context.Context, playerID string, initial money.Money) (*wallet.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
	w, err := wallet.New(id, playerID, initial, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", usecase.ErrInvalidInput, err)
	}
	key := playerID + "/" + initial.Currency()
	if f.players[key] {
		return nil, fmt.Errorf("%w: %s", usecase.ErrWalletAlreadyExists, key)
	}
	f.players[key] = true
	return w, nil
}

type processedWager struct {
	in  usecase.ProcessWagerInput
	out usecase.ProcessWagerOutput
}

type fakeProcessor struct {
	mu      sync.Mutex
	balance int64
	calls   int
	last    usecase.ProcessWagerInput
	byKey   map[string]processedWager
	seq     int
	err     error
}

func (f *fakeProcessor) ProcessWager(_ context.Context, in usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = in
	if f.err != nil {
		return usecase.ProcessWagerOutput{}, f.err
	}
	if prev, ok := f.byKey[in.IdempotencyKey]; ok {
		if prev.in != in {
			return usecase.ProcessWagerOutput{}, fmt.Errorf("%w: key %s", usecase.ErrIdempotencyConflict, in.IdempotencyKey)
		}
		replay := prev.out
		replay.Replayed = true
		return replay, nil
	}

	f.seq++
	out := usecase.ProcessWagerOutput{
		WalletID:      in.WalletID,
		TransactionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq),
		Kind:          in.Kind,
		Status:        wagering.StatusProcessed,
	}
	if in.Kind == wagering.KindBet {
		if in.Amount.Cents() > f.balance {
			out.Status = wagering.StatusRejected
			out.FailureCode = usecase.FailureInsufficientFunds
		} else {
			f.balance -= in.Amount.Cents()
		}
	}
	out.Balance = brl(f.balance)
	f.byKey[in.IdempotencyKey] = processedWager{in: in, out: out}
	return out, nil
}

type fakeReconciler struct{}

func (fakeReconciler) Reconcile(_ context.Context, walletID string) (usecase.ReconciliationReport, error) {
	if walletID != testWalletID {
		return usecase.ReconciliationReport{}, fmt.Errorf("%w: %s", usecase.ErrWalletNotFound, walletID)
	}
	return usecase.ReconciliationReport{
		WalletID:      walletID,
		Currency:      "BRL",
		Credits:       brl(10000),
		Debits:        brl(2500),
		LedgerBalance: brl(7500),
		WalletBalance: brl(7500),
		Difference:    brl(0),
		EntryCount:    2,
		Consistent:    true,
	}, nil
}

type fakeWallets struct{ items map[string]*wallet.Wallet }

func (f fakeWallets) GetByID(_ context.Context, _ postgres.Querier, id string) (*wallet.Wallet, error) {
	w, ok := f.items[id]
	if !ok {
		return nil, fmt.Errorf("%w: wallet %s", postgres.ErrNotFound, id)
	}
	return w, nil
}

type fakeLedgers struct {
	entries []ledger.Entry
	next    string
}

func (f fakeLedgers) ListByWalletCursor(_ context.Context, _ postgres.Querier, _, cursor string, _ int) ([]ledger.Entry, string, error) {
	if cursor == "bad" {
		return nil, "", fmt.Errorf("%w: test", postgres.ErrInvalidCursor)
	}
	return f.entries, f.next, nil
}

type fakeWagers struct{ items []*wagering.WagerTransaction }

func (f fakeWagers) GetByID(_ context.Context, _ postgres.Querier, id string) (*wagering.WagerTransaction, error) {
	for _, t := range f.items {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: transaction %s", postgres.ErrNotFound, id)
}

func (f fakeWagers) GetByProviderAndExternal(_ context.Context, _ postgres.Querier, providerID, externalID string) (*wagering.WagerTransaction, error) {
	for _, t := range f.items {
		if t.ProviderID == providerID && t.ExternalTransactionID == externalID {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: provider %s external %s", postgres.ErrNotFound, providerID, externalID)
}

type fakeDB struct {
	postgres.Querier // never used: the fake repositories ignore it
	pingErr          error
}

func (f *fakeDB) Ping(context.Context) error { return f.pingErr }

// ---------------------------------------------------------------- environment

type env struct {
	router    http.Handler
	opener    *fakeOpener
	processor *fakeProcessor
	db        *fakeDB
}

func fixtureTx(t *testing.T, id, provider, external string, now time.Time) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                    id,
		WalletID:              testWalletID,
		Kind:                  wagering.KindBet,
		Amount:                brl(2500),
		ProviderID:            provider,
		ExternalTransactionID: external,
		IdempotencyKey:        "idem-" + external,
		RequestHash:           "hash",
		RoundID:               "round-1",
		GameID:                "game-1",
		Now:                   now,
	})
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := tx.MarkProcessed(brl(7500)); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	return tx
}

func newEnv(t *testing.T) *env {
	t.Helper()
	now := time.Now().UTC()

	w, err := wallet.New(testWalletID, "player-1", brl(7500), now)
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	entry, err := ledger.New(testEntryID, testWalletID, ownTxID, ledger.DirectionDebit, brl(2500), brl(10000), brl(7500), now)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}

	e := &env{
		opener:    &fakeOpener{players: map[string]bool{}},
		processor: &fakeProcessor{balance: 10000, byKey: map[string]processedWager{}},
		db:        &fakeDB{},
	}
	e.router = newRouter(deps{
		opener:     e.opener,
		processor:  e.processor,
		reconciler: fakeReconciler{},
		wallets:    fakeWallets{items: map[string]*wallet.Wallet{testWalletID: w}},
		ledgers:    fakeLedgers{entries: []ledger.Entry{entry}, next: "next-cursor"},
		wagers: fakeWagers{items: []*wagering.WagerTransaction{
			fixtureTx(t, ownTxID, "provider-a", "ext-1", now),
			fixtureTx(t, otherTxID, "provider-b", "ext-2", now),
		}},
		db:           e.db,
		authenticate: fakeAuth,
		requireScope: fakeScope,
		providerID:   fakeProviderID,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

func call(t *testing.T, h http.Handler, method, path, token string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, rec.Body.String())
	}
	return m
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

func wagerBody(walletID, external, kind, amount string) map[string]any {
	return map[string]any{
		"walletId":              walletID,
		"externalTransactionId": external,
		"kind":                  kind,
		"amount":                map[string]string{"amount": amount, "currency": "BRL"},
		"roundId":               "round-1",
		"gameId":                "game-1",
	}
}

func idem(key string) map[string]string { return map[string]string{headerIdemKey: key} }

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, want, rec.Body.String())
	}
}

func expectErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := dig(decodeMap(t, rec), "error", "code"); got != want {
		t.Fatalf("error code = %v, want %s (body: %s)", got, want, rec.Body.String())
	}
}

// ---------------------------------------------------------------- tests

func TestHealth(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/health/live", "", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	if got := dig(decodeMap(t, rec), "status"); got != "ok" {
		t.Fatalf("live status = %v", got)
	}

	rec = call(t, e.router, http.MethodGet, "/health/ready", "", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	if got := dig(decodeMap(t, rec), "status"); got != "ready" {
		t.Fatalf("ready status = %v", got)
	}

	e.db.pingErr = errors.New("connection refused")
	rec = call(t, e.router, http.MethodGet, "/health/ready", "", nil, nil)
	expectStatus(t, rec, http.StatusServiceUnavailable)
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatal("readiness response must not leak the error")
	}
}

func TestProtectedRoutesRejectMissingAndInvalidTokens(t *testing.T) {
	e := newEnv(t)
	routes := []struct{ method, path string }{
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/" + testWalletID},
		{http.MethodGet, "/wallets/" + testWalletID + "/ledger"},
		{http.MethodPost, "/wallets/" + testWalletID + "/reconciliation"},
		{http.MethodPost, "/wagering/transactions"},
		{http.MethodGet, "/wagering/transactions/" + ownTxID},
		{http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1"},
	}
	for _, rt := range routes {
		for _, token := range []string{"", "invalid-token"} {
			rec := call(t, e.router, rt.method, rt.path, token, idem("k"), "{}")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with token %q: status = %d, want 401", rt.method, rt.path, token, rec.Code)
			}
		}
	}
	if e.processor.calls != 0 || len(e.opener.players) != 0 {
		t.Fatal("no use case may run for unauthenticated requests")
	}
}

func TestScopes(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodPost, "/wallets", "token-provider-a", nil,
		map[string]any{"playerId": "p1", "initialBalance": map[string]string{"amount": "10.00", "currency": "BRL"}})
	expectStatus(t, rec, http.StatusForbidden)

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-internal", idem("k-scope"),
		wagerBody(testWalletID, "ext-s", "BET", "1.00"))
	expectStatus(t, rec, http.StatusForbidden)
	if e.processor.calls != 0 {
		t.Fatal("the processor must not run without the wager.write scope")
	}
}

func TestCreateWallet(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"playerId": "player-9", "initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"}}

	rec := call(t, e.router, http.MethodPost, "/wallets", "token-internal", nil, body)
	expectStatus(t, rec, http.StatusCreated)
	m := decodeMap(t, rec)
	if dig(m, "playerId") != "player-9" || dig(m, "balance", "amount") != "100.00" || dig(m, "balance", "currency") != "BRL" ||
		dig(m, "currency") != "BRL" || dig(m, "version") != float64(1) {
		t.Fatalf("unexpected wallet: %s", rec.Body.String())
	}
	id, _ := dig(m, "id").(string)
	if id == "" || rec.Header().Get("Location") != "/wallets/"+id {
		t.Fatalf("id=%q location=%q", id, rec.Header().Get("Location"))
	}

	rec = call(t, e.router, http.MethodPost, "/wallets", "token-internal", nil, body)
	expectStatus(t, rec, http.StatusConflict)
	expectErrorCode(t, rec, "wallet_already_exists")

	invalid := map[string]struct {
		body any
		code string
	}{
		"missing initial balance": {map[string]any{"playerId": "p1"}, "invalid_input"},
		"missing player":          {map[string]any{"initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}, "invalid_input"},
		"negative balance": {map[string]any{"playerId": "p2", "initialBalance": map[string]string{"amount": "-1.00", "currency": "BRL"}},
			"invalid_input"},
		"scientific amount": {map[string]any{"playerId": "p3", "initialBalance": map[string]string{"amount": "1e3", "currency": "BRL"}},
			"invalid_json"},
		"amount as number": {`{"playerId":"p4","initialBalance":{"amount":10.5,"currency":"BRL"}}`, "invalid_json"},
		"unknown field":    {map[string]any{"playerId": "p5", "extra": 1, "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}, "invalid_json"},
		"malformed json":   {`{"playerId":`, "invalid_json"},
		"trailing data":    {`{"playerId":"p6","initialBalance":{"amount":"1.00","currency":"BRL"}} {}`, "invalid_json"},
	}
	for name, c := range invalid {
		rec := call(t, e.router, http.MethodPost, "/wallets", "token-internal", nil, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body: %s)", name, rec.Code, rec.Body.String())
		}
		expectErrorCode(t, rec, c.code)
	}
}

func TestProcessWagerSuccess(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-1"),
		wagerBody(testWalletID, "ext-100", "BET", "25.00"))
	expectStatus(t, rec, http.StatusCreated)
	m := decodeMap(t, rec)
	if id, _ := dig(m, "transactionId").(string); id == "" {
		t.Fatalf("missing transactionId: %s", rec.Body.String())
	}
	if dig(m, "status") != "PROCESSED" || dig(m, "balance", "amount") != "75.00" || dig(m, "idempotentReplay") != false {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}

	if e.processor.last.ProviderID != "provider-a" || e.processor.last.IdempotencyKey != "k-1" ||
		e.processor.last.Kind != wagering.KindBet || e.processor.last.Amount.Cents() != 2500 ||
		e.processor.last.WalletID != testWalletID || e.processor.last.RoundID != "round-1" {
		t.Fatalf("processor received %+v", e.processor.last)
	}
}

func TestProcessWagerReplay(t *testing.T) {
	e := newEnv(t)
	body := wagerBody(testWalletID, "ext-101", "BET", "25.00")

	first := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-replay"), body)
	expectStatus(t, first, http.StatusCreated)
	second := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-replay"), body)
	expectStatus(t, second, http.StatusOK)

	a, b := decodeMap(t, first), decodeMap(t, second)
	if dig(b, "idempotentReplay") != true || dig(a, "idempotentReplay") != false {
		t.Fatalf("replay flags: first=%v second=%v", dig(a, "idempotentReplay"), dig(b, "idempotentReplay"))
	}
	if dig(a, "transactionId") != dig(b, "transactionId") || dig(a, "balance", "amount") != dig(b, "balance", "amount") {
		t.Fatalf("replay must return the original result: %s vs %s", first.Body.String(), second.Body.String())
	}
}

func TestProcessWagerIdempotencyConflict(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-conflict"),
		wagerBody(testWalletID, "ext-102", "BET", "25.00"))
	expectStatus(t, rec, http.StatusCreated)

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-conflict"),
		wagerBody(testWalletID, "ext-102", "BET", "26.00"))
	expectStatus(t, rec, http.StatusConflict)
	expectErrorCode(t, rec, "idempotency_conflict")
}

func TestProcessWagerRequiresIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	for name, headers := range map[string]map[string]string{
		"absent": nil,
		"blank":  {headerIdemKey: "   "},
	} {
		rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", headers,
			wagerBody(testWalletID, "ext-103", "BET", "1.00"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec.Code)
		}
		expectErrorCode(t, rec, "missing_idempotency_key")
	}
	if e.processor.calls != 0 {
		t.Fatal("the processor must not run without an idempotency key")
	}
}

func TestProcessWagerProviderComesFromToken(t *testing.T) {
	e := newEnv(t)

	spoofed := wagerBody(testWalletID, "ext-104", "BET", "1.00")
	spoofed["providerId"] = "provider-b"
	rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-spoof"), spoofed)
	expectStatus(t, rec, http.StatusForbidden)
	expectErrorCode(t, rec, "provider_mismatch")
	if e.processor.calls != 0 {
		t.Fatal("a spoofed provider must never reach the processor")
	}

	same := wagerBody(testWalletID, "ext-105", "BET", "1.00")
	same["providerId"] = "provider-a"
	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-same"), same)
	expectStatus(t, rec, http.StatusCreated)

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-no-provider", idem("k-nop"),
		wagerBody(testWalletID, "ext-106", "BET", "1.00"))
	expectStatus(t, rec, http.StatusForbidden)
	if e.processor.calls != 1 {
		t.Fatalf("processor calls = %d, want 1", e.processor.calls)
	}

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-b", idem("k-b"),
		wagerBody(testWalletID, "ext-107", "BET", "1.00"))
	expectStatus(t, rec, http.StatusCreated)
	if e.processor.last.ProviderID != "provider-b" {
		t.Fatalf("provider = %s, want provider-b", e.processor.last.ProviderID)
	}
}

func TestProcessWagerRejectedIs422(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-reject"),
		wagerBody(testWalletID, "ext-108", "BET", "500.00"))
	expectStatus(t, rec, http.StatusUnprocessableEntity)
	m := decodeMap(t, rec)
	if dig(m, "status") != "REJECTED" || dig(m, "failureCode") != "INSUFFICIENT_FUNDS" ||
		dig(m, "balance", "amount") != "100.00" || dig(m, "idempotentReplay") != false {
		t.Fatalf("unexpected rejected response: %s", rec.Body.String())
	}

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-reject"),
		wagerBody(testWalletID, "ext-108", "BET", "500.00"))
	expectStatus(t, rec, http.StatusUnprocessableEntity)
	if dig(decodeMap(t, rec), "idempotentReplay") != true {
		t.Fatal("replay of a rejected transaction must be flagged as replay")
	}
}

func TestProcessWagerValidation(t *testing.T) {
	e := newEnv(t)
	noAmount := wagerBody(testWalletID, "ext-109", "BET", "1.00")
	delete(noAmount, "amount")

	rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-noamount"), noAmount)
	expectStatus(t, rec, http.StatusBadRequest)

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-unknown"),
		`{"walletId":"`+testWalletID+`","extra":true}`)
	expectStatus(t, rec, http.StatusBadRequest)
	expectErrorCode(t, rec, "invalid_json")

	rec = call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-empty"), "")
	expectStatus(t, rec, http.StatusBadRequest)

	if e.processor.calls != 0 {
		t.Fatal("invalid bodies must not reach the processor")
	}
}

func TestProcessWagerErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid input", fmt.Errorf("%w: bad", usecase.ErrInvalidInput), http.StatusBadRequest, "invalid_input"},
		{"wallet not found", fmt.Errorf("%w: x", usecase.ErrWalletNotFound), http.StatusNotFound, "not_found"},
		{"idempotency conflict", fmt.Errorf("%w: x", usecase.ErrIdempotencyConflict), http.StatusConflict, "idempotency_conflict"},
		{"duplicate external", fmt.Errorf("%w: x", usecase.ErrDuplicateExternalTransaction), http.StatusConflict, "duplicate_external_transaction"},
		{"deadline", context.DeadlineExceeded, http.StatusServiceUnavailable, "unavailable"},
		{"unexpected", errors.New("db exploded: password=secret"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.processor.err = tc.err
			rec := call(t, e.router, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-err"),
				wagerBody(testWalletID, "ext-err", "BET", "1.00"))
			expectStatus(t, rec, tc.wantStatus)
			expectErrorCode(t, rec, tc.wantCode)
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatal("unexpected errors must not leak details")
			}
		})
	}
}

func TestProcessWagerPendingReferenceIs202(t *testing.T) {
	e := newEnv(t)
	e.processor.byKey["k-pending"] = processedWager{}
	// A dedicated processor result: use the error-free path with a custom fake.
	h := newRouter(deps{
		opener:       e.opener,
		processor:    pendingProcessor{},
		reconciler:   fakeReconciler{},
		wallets:      fakeWallets{},
		ledgers:      fakeLedgers{},
		wagers:       fakeWagers{},
		db:           e.db,
		authenticate: fakeAuth,
		requireScope: fakeScope,
		providerID:   fakeProviderID,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	body := wagerBody(testWalletID, "ext-p", "REFUND", "1.00")
	body["referenceExternalTransactionId"] = "ext-unknown"
	rec := call(t, h, http.MethodPost, "/wagering/transactions", "token-provider-a", idem("k-pending"), body)
	expectStatus(t, rec, http.StatusAccepted)
	if dig(decodeMap(t, rec), "status") != "PENDING_REFERENCE" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

type pendingProcessor struct{}

func (pendingProcessor) ProcessWager(_ context.Context, in usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error) {
	return usecase.ProcessWagerOutput{
		WalletID:      in.WalletID,
		TransactionID: "00000000-0000-4000-8000-000000000777",
		Kind:          in.Kind,
		Status:        wagering.StatusPendingReference,
		Balance:       brl(7500),
	}, nil
}

func TestGetWallet(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/wallets/"+testWalletID, "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	if dig(m, "id") != testWalletID || dig(m, "balance", "amount") != "75.00" || dig(m, "playerId") != "player-1" {
		t.Fatalf("unexpected wallet: %s", rec.Body.String())
	}

	rec = call(t, e.router, http.MethodGet, "/wallets/"+unknownID, "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)
	expectErrorCode(t, rec, "not_found")

	rec = call(t, e.router, http.MethodGet, "/wallets/not-a-uuid", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusBadRequest)
	expectErrorCode(t, rec, "invalid_input")
}

func TestListLedger(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/wallets/"+testWalletID+"/ledger?limit=10", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	items, _ := dig(m, "items").([]any)
	if len(items) != 1 || dig(m, "nextCursor") != "next-cursor" {
		t.Fatalf("unexpected page: %s", rec.Body.String())
	}
	first, _ := items[0].(map[string]any)
	if dig(first, "direction") != "DEBIT" || dig(first, "amount", "amount") != "25.00" ||
		dig(first, "balanceBefore", "amount") != "100.00" || dig(first, "balanceAfter", "amount") != "75.00" ||
		dig(first, "transactionId") != ownTxID {
		t.Fatalf("unexpected entry: %v", first)
	}

	for name, query := range map[string]string{"zero limit": "?limit=0", "text limit": "?limit=abc", "negative limit": "?limit=-5"} {
		rec := call(t, e.router, http.MethodGet, "/wallets/"+testWalletID+"/ledger"+query, "token-provider-a", nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec.Code)
		}
	}

	rec = call(t, e.router, http.MethodGet, "/wallets/"+testWalletID+"/ledger?cursor=bad", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusBadRequest)
	expectErrorCode(t, rec, "invalid_cursor")

	rec = call(t, e.router, http.MethodGet, "/wallets/"+unknownID+"/ledger", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)
}

func TestReconciliation(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodPost, "/wallets/"+testWalletID+"/reconciliation", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	if dig(m, "consistent") != true || dig(m, "ledgerBalance", "amount") != "75.00" ||
		dig(m, "walletBalance", "amount") != "75.00" || dig(m, "difference", "amount") != "0.00" ||
		dig(m, "entryCount") != float64(2) {
		t.Fatalf("unexpected report: %s", rec.Body.String())
	}

	rec = call(t, e.router, http.MethodPost, "/wallets/"+unknownID+"/reconciliation", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)

	rec = call(t, e.router, http.MethodPost, "/wallets/nope/reconciliation", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusBadRequest)
}

func TestGetTransaction(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/wagering/transactions/"+ownTxID, "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	if dig(m, "id") != ownTxID || dig(m, "status") != "PROCESSED" || dig(m, "kind") != "BET" ||
		dig(m, "amount", "amount") != "25.00" || dig(m, "balance", "amount") != "75.00" ||
		dig(m, "providerId") != "provider-a" || dig(m, "externalTransactionId") != "ext-1" {
		t.Fatalf("unexpected transaction: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "requestHash") || strings.Contains(rec.Body.String(), "idempotencyKey") {
		t.Fatal("internal fields must not be exposed")
	}

	rec = call(t, e.router, http.MethodGet, "/wagering/transactions/"+otherTxID, "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)

	rec = call(t, e.router, http.MethodGet, "/wagering/transactions/"+unknownID, "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)

	rec = call(t, e.router, http.MethodGet, "/wagering/transactions/"+ownTxID, "token-internal", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)

	rec = call(t, e.router, http.MethodGet, "/wagering/transactions/xyz", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusBadRequest)
}

func TestGetTransactionByExternal(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusOK)
	if dig(decodeMap(t, rec), "id") != ownTxID {
		t.Fatalf("unexpected transaction: %s", rec.Body.String())
	}

	rec = call(t, e.router, http.MethodGet, "/providers/provider-b/wagering/transactions/ext-2", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusForbidden)

	rec = call(t, e.router, http.MethodGet, "/providers/provider-a/wagering/transactions/missing", "token-provider-a", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)

	rec = call(t, e.router, http.MethodGet, "/providers/provider-a/wagering/transactions/ext-1", "token-no-provider", nil, nil)
	expectStatus(t, rec, http.StatusForbidden)
}

func TestUnknownRouteAndMethod(t *testing.T) {
	e := newEnv(t)

	rec := call(t, e.router, http.MethodGet, "/nope", "", nil, nil)
	expectStatus(t, rec, http.StatusNotFound)
	expectErrorCode(t, rec, "not_found")

	rec = call(t, e.router, http.MethodDelete, "/health/live", "", nil, nil)
	expectStatus(t, rec, http.StatusMethodNotAllowed)
	expectErrorCode(t, rec, "method_not_allowed")
}
