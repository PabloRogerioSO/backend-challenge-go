package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func mustCents(t *testing.T, cents int64, currency string) Money {
	t.Helper()
	m, err := FromCents(cents, currency)
	if err != nil {
		t.Fatalf("FromCents(%d, %q) unexpected error: %v", cents, currency, err)
	}
	return m
}

func TestParseMoney(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		currency string
		want     int64
		wantErr  error
	}{
		{"integer", "10", "BRL", 1000, nil},
		{"zero", "0", "BRL", 0, nil},
		{"zero with scale", "0.00", "USD", 0, nil},
		{"one fraction digit", "10.5", "BRL", 1050, nil},
		{"two fraction digits", "10.50", "BRL", 1050, nil},
		{"one cent", "0.01", "EUR", 1, nil},
		{"negative", "-10.50", "BRL", -1050, nil},
		{"negative one cent", "-0.01", "BRL", -1, nil},
		{"negative zero", "-0", "BRL", 0, nil},
		{"leading zeros", "007.10", "BRL", 710, nil},
		{"max int64", "92233720368547758.07", "BRL", math.MaxInt64, nil},
		{"min int64", "-92233720368547758.08", "BRL", math.MinInt64, nil},

		{"empty", "", "BRL", 0, ErrInvalidMoney},
		{"scientific lower", "1e3", "BRL", 0, ErrInvalidMoney},
		{"scientific upper", "1E3", "BRL", 0, ErrInvalidMoney},
		{"scientific with fraction", "1.5e2", "BRL", 0, ErrInvalidMoney},
		{"NaN", "NaN", "BRL", 0, ErrInvalidMoney},
		{"Infinity", "Infinity", "BRL", 0, ErrInvalidMoney},
		{"negative Infinity", "-Infinity", "BRL", 0, ErrInvalidMoney},
		{"inf", "inf", "BRL", 0, ErrInvalidMoney},
		{"plus sign", "+1", "BRL", 0, ErrInvalidMoney},
		{"no integer part", ".5", "BRL", 0, ErrInvalidMoney},
		{"trailing dot", "5.", "BRL", 0, ErrInvalidMoney},
		{"scale 3", "1.234", "BRL", 0, ErrInvalidMoney},
		{"scale 3 zeros", "1.000", "BRL", 0, ErrInvalidMoney},
		{"comma separator", "1,50", "BRL", 0, ErrInvalidMoney},
		{"double minus", "--1", "BRL", 0, ErrInvalidMoney},
		{"two dots", "1.0.0", "BRL", 0, ErrInvalidMoney},
		{"hex", "0x10", "BRL", 0, ErrInvalidMoney},
		{"leading space", " 1", "BRL", 0, ErrInvalidMoney},
		{"trailing space", "1 ", "BRL", 0, ErrInvalidMoney},
		{"trailing newline", "1\n", "BRL", 0, ErrInvalidMoney},
		{"arabic digits", "١٢", "BRL", 0, ErrInvalidMoney},
		{"underscore", "1_000", "BRL", 0, ErrInvalidMoney},
		{"lone minus", "-", "BRL", 0, ErrInvalidMoney},

		{"overflow positive", "92233720368547758.08", "BRL", 0, ErrOverflow},
		{"overflow negative", "-92233720368547758.09", "BRL", 0, ErrOverflow},
		{"overflow huge", "99999999999999999999", "BRL", 0, ErrOverflow},

		{"unknown currency", "1", "XXX", 0, ErrInvalidMoney},
		{"lowercase currency", "1", "brl", 0, ErrInvalidMoney},
		{"empty currency", "1", "", 0, ErrInvalidMoney},
		{"short currency", "1", "BR", 0, ErrInvalidMoney},
		{"long currency", "1", "BRLL", 0, ErrInvalidMoney},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMoney(tc.amount, tc.currency)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseMoney(%q, %q) error = %v, want %v", tc.amount, tc.currency, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMoney(%q, %q) unexpected error: %v", tc.amount, tc.currency, err)
			}
			if got.Cents() != tc.want || got.Currency() != tc.currency {
				t.Fatalf("ParseMoney(%q, %q) = %d %s, want %d %s", tc.amount, tc.currency, got.Cents(), got.Currency(), tc.want, tc.currency)
			}
		})
	}
}

func TestFromCentsAndZero(t *testing.T) {
	m, err := FromCents(-250, "USD")
	if err != nil {
		t.Fatalf("FromCents unexpected error: %v", err)
	}
	if m.Cents() != -250 || m.Currency() != "USD" {
		t.Fatalf("FromCents = %v", m)
	}
	if _, err := FromCents(1, "ZZZ"); !errors.Is(err, ErrInvalidMoney) {
		t.Fatalf("FromCents unknown currency error = %v, want ErrInvalidMoney", err)
	}
	z := Zero("BRL")
	if !z.IsZero() || z.Currency() != "BRL" || z.Sign() != 0 {
		t.Fatalf("Zero(BRL) = %v", z)
	}
}

func TestAddSub(t *testing.T) {
	a := mustCents(t, 1000, "BRL")
	b := mustCents(t, 250, "BRL")
	usd := mustCents(t, 250, "USD")

	sum, err := a.Add(b)
	if err != nil || sum.Cents() != 1250 {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Cents() != 750 {
		t.Fatalf("Sub = %v, %v", diff, err)
	}
	negative, err := b.Sub(a)
	if err != nil || negative.Cents() != -750 {
		t.Fatalf("Sub negative = %v, %v", negative, err)
	}
	if a.Cents() != 1000 || b.Cents() != 250 {
		t.Fatalf("operands were mutated: %v %v", a, b)
	}

	if _, err := a.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add mismatch error = %v", err)
	}
	if _, err := a.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub mismatch error = %v", err)
	}
}

func TestOverflow(t *testing.T) {
	maxM := mustCents(t, math.MaxInt64, "BRL")
	minM := mustCents(t, math.MinInt64, "BRL")
	one := mustCents(t, 1, "BRL")
	minusOne := mustCents(t, -1, "BRL")

	tests := []struct {
		name string
		op   func() (Money, error)
	}{
		{"max + 1", func() (Money, error) { return maxM.Add(one) }},
		{"min + -1", func() (Money, error) { return minM.Add(minusOne) }},
		{"min - 1", func() (Money, error) { return minM.Sub(one) }},
		{"max - -1", func() (Money, error) { return maxM.Sub(minusOne) }},
		{"negate min", func() (Money, error) { return minM.Negate() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.op(); !errors.Is(err, ErrOverflow) {
				t.Fatalf("error = %v, want ErrOverflow", err)
			}
		})
	}

	if got, err := maxM.Add(minusOne); err != nil || got.Cents() != math.MaxInt64-1 {
		t.Fatalf("max + -1 = %v, %v", got, err)
	}
	if got, err := minM.Add(one); err != nil || got.Cents() != math.MinInt64+1 {
		t.Fatalf("min + 1 = %v, %v", got, err)
	}
}

func TestNegate(t *testing.T) {
	m := mustCents(t, 1050, "BRL")
	n, err := m.Negate()
	if err != nil || n.Cents() != -1050 || n.Currency() != "BRL" {
		t.Fatalf("Negate = %v, %v", n, err)
	}
	z, err := Zero("BRL").Negate()
	if err != nil || !z.IsZero() {
		t.Fatalf("Negate zero = %v, %v", z, err)
	}
}

func TestCompareEquals(t *testing.T) {
	one := mustCents(t, 100, "BRL")
	two := mustCents(t, 200, "BRL")
	usd := mustCents(t, 100, "USD")

	tests := []struct {
		name string
		a, b Money
		want int
	}{
		{"less", one, two, -1},
		{"greater", two, one, 1},
		{"equal", one, one, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.a.Compare(tc.b)
			if err != nil || got != tc.want {
				t.Fatalf("Compare = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
	if _, err := one.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Compare mismatch error = %v", err)
	}
	if !one.Equals(mustCents(t, 100, "BRL")) {
		t.Fatal("Equals same value = false")
	}
	if one.Equals(two) || one.Equals(usd) {
		t.Fatal("Equals different value or currency = true")
	}
}

func TestPredicates(t *testing.T) {
	tests := []struct {
		cents    int64
		neg, zro bool
		sign     int
	}{
		{-5, true, false, -1},
		{0, false, true, 0},
		{5, false, false, 1},
	}
	for _, tc := range tests {
		m := mustCents(t, tc.cents, "BRL")
		if m.IsNegative() != tc.neg || m.IsZero() != tc.zro || m.Sign() != tc.sign {
			t.Fatalf("predicates for %d: neg=%v zero=%v sign=%d", tc.cents, m.IsNegative(), m.IsZero(), m.Sign())
		}
	}
}

func TestStringAndAmount(t *testing.T) {
	tests := []struct {
		cents      int64
		wantAmount string
		wantString string
	}{
		{1050, "10.50", "10.50 BRL"},
		{0, "0.00", "0.00 BRL"},
		{5, "0.05", "0.05 BRL"},
		{-1, "-0.01", "-0.01 BRL"},
		{-100000, "-1000.00", "-1000.00 BRL"},
		{math.MaxInt64, "92233720368547758.07", "92233720368547758.07 BRL"},
		{math.MinInt64, "-92233720368547758.08", "-92233720368547758.08 BRL"},
	}
	for _, tc := range tests {
		m := mustCents(t, tc.cents, "BRL")
		if m.Amount() != tc.wantAmount || m.String() != tc.wantString {
			t.Fatalf("cents %d: Amount=%q String=%q, want %q / %q", tc.cents, m.Amount(), m.String(), tc.wantAmount, tc.wantString)
		}
	}
}

func TestMarshalJSON(t *testing.T) {
	data, err := json.Marshal(mustCents(t, 1050, "BRL"))
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(data) != `{"amount":"10.50","currency":"BRL"}` {
		t.Fatalf("Marshal = %s", data)
	}
	if _, err := json.Marshal(Money{}); !errors.Is(err, ErrInvalidMoney) {
		t.Fatalf("Marshal zero value error = %v, want ErrInvalidMoney", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	type payload struct {
		Stake Money `json:"stake"`
		Fee   Money `json:"fee"`
	}
	values := []Money{
		mustCents(t, 0, "BRL"),
		mustCents(t, 1, "USD"),
		mustCents(t, -1050, "EUR"),
		mustCents(t, math.MaxInt64, "BRL"),
		mustCents(t, math.MinInt64, "BRL"),
	}
	for _, v := range values {
		in := payload{Stake: v, Fee: mustCents(t, 99, "BRL")}
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(%v) error: %v", v, err)
		}
		var out payload
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("Unmarshal(%s) error: %v", data, err)
		}
		if !out.Stake.Equals(in.Stake) || !out.Fee.Equals(in.Fee) {
			t.Fatalf("round trip %s = %+v, want %+v", data, out, in)
		}
	}
}

func TestUnmarshalJSONInvalid(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"null", `null`, ErrInvalidMoney},
		{"empty object", `{}`, ErrInvalidMoney},
		{"bare string", `"10.50"`, ErrInvalidMoney},
		{"amount as number", `{"amount":10.5,"currency":"BRL"}`, ErrInvalidMoney},
		{"scientific amount", `{"amount":"1e3","currency":"BRL"}`, ErrInvalidMoney},
		{"scale 3", `{"amount":"10.555","currency":"BRL"}`, ErrInvalidMoney},
		{"unknown currency", `{"amount":"10.50","currency":"XXX"}`, ErrInvalidMoney},
		{"lowercase currency", `{"amount":"10.50","currency":"brl"}`, ErrInvalidMoney},
		{"missing currency", `{"amount":"10.50"}`, ErrInvalidMoney},
		{"unknown field", `{"amount":"10.50","currency":"BRL","extra":1}`, ErrInvalidMoney},
		{"overflow", `{"amount":"92233720368547758.08","currency":"BRL"}`, ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m Money
			err := json.Unmarshal([]byte(tc.in), &m)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unmarshal(%s) error = %v, want %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
