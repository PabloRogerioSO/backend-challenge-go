package httpapi

import (
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
)

// ---------------------------------------------------------------- requests

type createWalletRequest struct {
	PlayerID       string       `json:"playerId"`
	InitialBalance *money.Money `json:"initialBalance"`
}

type processWagerRequest struct {
	// ProviderID is optional and informative only: the provider always comes from
	// the token, and a different value in the body is rejected with 403.
	ProviderID                     string       `json:"providerId,omitempty"`
	WalletID                       string       `json:"walletId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	Kind                           string       `json:"kind"`
	Amount                         *money.Money `json:"amount"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
}

// ---------------------------------------------------------------- responses

type walletResponse struct {
	ID        string      `json:"id"`
	PlayerID  string      `json:"playerId"`
	Currency  string      `json:"currency"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

func newWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{
		ID:        w.ID(),
		PlayerID:  w.PlayerID(),
		Currency:  w.Currency(),
		Balance:   w.Balance(),
		Version:   w.Version(),
		CreatedAt: w.CreatedAt(),
		UpdatedAt: w.UpdatedAt(),
	}
}

type processWagerResponse struct {
	TransactionID    string      `json:"transactionId"`
	WalletID         string      `json:"walletId"`
	Kind             string      `json:"kind"`
	Status           string      `json:"status"`
	FailureCode      string      `json:"failureCode,omitempty"`
	Balance          money.Money `json:"balance"`
	IdempotentReplay bool        `json:"idempotentReplay"`
}

func newProcessWagerResponse(out usecase.ProcessWagerOutput) processWagerResponse {
	return processWagerResponse{
		TransactionID:    out.TransactionID,
		WalletID:         out.WalletID,
		Kind:             string(out.Kind),
		Status:           string(out.Status),
		FailureCode:      out.FailureCode,
		Balance:          out.Balance,
		IdempotentReplay: out.Replayed,
	}
}

type transactionResponse struct {
	ID                             string       `json:"id"`
	WalletID                       string       `json:"walletId"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	Kind                           string       `json:"kind"`
	Status                         string       `json:"status"`
	Amount                         money.Money  `json:"amount"`
	Balance                        *money.Money `json:"balance,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ResolvedReferenceTransactionID string       `json:"resolvedReferenceTransactionId,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
}

func newTransactionResponse(t *wagering.WagerTransaction) transactionResponse {
	return transactionResponse{
		ID:                             t.ID,
		WalletID:                       t.WalletID,
		ProviderID:                     t.ProviderID,
		ExternalTransactionID:          t.ExternalTransactionID,
		Kind:                           string(t.Kind),
		Status:                         string(t.Status),
		Amount:                         t.Amount,
		Balance:                        t.ResultBalance,
		FailureCode:                    t.FailureCode,
		RoundID:                        t.RoundID,
		GameID:                         t.GameID,
		ReferenceExternalTransactionID: t.ReferenceExternalTransactionID,
		ResolvedReferenceTransactionID: t.ResolvedReferenceTransactionID,
		CreatedAt:                      t.CreatedAt,
		UpdatedAt:                      t.UpdatedAt,
	}
}

type ledgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Amount        money.Money `json:"amount"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerPageResponse struct {
	Items      []ledgerEntryResponse `json:"items"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func newLedgerPageResponse(entries []ledger.Entry, next string) ledgerPageResponse {
	items := make([]ledgerEntryResponse, 0, len(entries))
	for _, e := range entries {
		items = append(items, ledgerEntryResponse{
			ID:            e.ID(),
			TransactionID: e.TransactionID(),
			Direction:     string(e.Direction()),
			Amount:        e.Amount(),
			BalanceBefore: e.BalanceBefore(),
			BalanceAfter:  e.BalanceAfter(),
			CreatedAt:     e.CreatedAt(),
		})
	}
	return ledgerPageResponse{Items: items, NextCursor: next}
}

type reconciliationResponse struct {
	WalletID      string      `json:"walletId"`
	Currency      string      `json:"currency"`
	Credits       money.Money `json:"credits"`
	Debits        money.Money `json:"debits"`
	LedgerBalance money.Money `json:"ledgerBalance"`
	WalletBalance money.Money `json:"walletBalance"`
	Difference    money.Money `json:"difference"`
	EntryCount    int64       `json:"entryCount"`
	Consistent    bool        `json:"consistent"`
}

func newReconciliationResponse(r usecase.ReconciliationReport) reconciliationResponse {
	return reconciliationResponse{
		WalletID:      r.WalletID,
		Currency:      r.Currency,
		Credits:       r.Credits,
		Debits:        r.Debits,
		LedgerBalance: r.LedgerBalance,
		WalletBalance: r.WalletBalance,
		Difference:    r.Difference,
		EntryCount:    r.EntryCount,
		Consistent:    r.Consistent,
	}
}

type healthResponse struct {
	Status string `json:"status"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}
