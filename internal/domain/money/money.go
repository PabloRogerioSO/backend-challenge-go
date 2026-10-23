// Package money implements an immutable monetary value object: an int64 amount
// in cents plus an ISO 4217 currency code. Floating point is never used.
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrInvalidMoney is returned for malformed amounts or unknown currencies.
	ErrInvalidMoney = errors.New("money: invalid value")
	// ErrCurrencyMismatch is returned when two values of different currencies are combined.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a value or an operation does not fit in int64 cents.
	ErrOverflow = errors.New("money: overflow")
)

var amountPattern = regexp.MustCompile(`^-?\d+(\.\d{1,2})?$`)

// iso4217Codes lists the active ISO 4217 alphabetic codes. Every currency is
// treated as having 2 decimal places, as required by the amount grammar.
const iso4217Codes = `AED AFN ALL AMD ANG AOA ARS AUD AWG AZN BAM BBD BDT BGN BHD BIF BMD BND BOB BRL BSD BTN BWP BYN BZD
CAD CDF CHF CLP CNY COP CRC CUP CVE CZK DJF DKK DOP DZD EGP ERN ETB EUR FJD FKP GBP GEL GHS GIP GMD GNF GTQ GYD
HKD HNL HTG HUF IDR ILS INR IQD IRR ISK JMD JOD JPY KES KGS KHR KMF KPW KRW KWD KYD KZT LAK LBP LKR LRD LSL LYD
MAD MDL MGA MKD MMK MNT MOP MRU MUR MVR MWK MXN MYR MZN NAD NGN NIO NOK NPR NZD OMR PAB PEN PGK PHP PKR PLN PYG
QAR RON RSD RUB RWF SAR SBD SCR SDG SEK SGD SHP SLE SOS SRD SSP STN SVC SYP SZL THB TJS TMT TND TOP TRY TTD TWD
TZS UAH UGX USD UYU UZS VED VES VND VUV WST XAF XCD XOF XPF YER ZAR ZMW ZWL`

var knownCurrencies = func() map[string]struct{} {
	set := make(map[string]struct{})
	for _, code := range strings.Fields(iso4217Codes) {
		set[code] = struct{}{}
	}
	return set
}()

// Money is an immutable amount of money in cents of a given currency.
// The zero value has no currency and is not a valid monetary value; build
// values with ParseMoney, FromCents or Zero.
type Money struct {
	cents    int64
	currency string
}

func validateCurrency(currency string) error {
	if _, ok := knownCurrencies[currency]; !ok {
		return fmt.Errorf("%w: unknown currency %q", ErrInvalidMoney, currency)
	}
	return nil
}

// ParseMoney parses a plain decimal string (at most 2 fraction digits) into Money.
func ParseMoney(amount, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}
	if !amountPattern.MatchString(amount) {
		return Money{}, fmt.Errorf("%w: amount %q is not a plain decimal with at most 2 fraction digits", ErrInvalidMoney, amount)
	}
	sign := ""
	digits := amount
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}
	intPart, frac, _ := strings.Cut(digits, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	cents, err := strconv.ParseInt(sign+intPart+frac, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return Money{}, fmt.Errorf("%w: amount %q", ErrOverflow, amount)
		}
		return Money{}, fmt.Errorf("%w: amount %q", ErrInvalidMoney, amount)
	}
	return Money{cents: cents, currency: currency}, nil
}

// FromCents builds Money from an integer number of cents.
func FromCents(cents int64, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{cents: cents, currency: currency}, nil
}

// Zero returns a zero amount in the given currency. It does not validate the
// currency; use FromCents(0, currency) when validation is needed.
func Zero(currency string) Money {
	return Money{currency: currency}
}

// Cents returns the amount in cents.
func (m Money) Cents() int64 { return m.cents }

// Currency returns the ISO 4217 code.
func (m Money) Currency() string { return m.currency }

func (m Money) sameCurrency(o Money) error {
	if m.currency != o.currency {
		return fmt.Errorf("%w: %q vs %q", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.cents > 0 && m.cents > math.MaxInt64-o.cents) || (o.cents < 0 && m.cents < math.MinInt64-o.cents) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{cents: m.cents + o.cents, currency: m.currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.cents < 0 && m.cents > math.MaxInt64+o.cents) || (o.cents > 0 && m.cents < math.MinInt64+o.cents) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{cents: m.cents - o.cents, currency: m.currency}, nil
}

// Negate returns -m.
func (m Money) Negate() (Money, error) {
	if m.cents == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: negate %s", ErrOverflow, m)
	}
	return Money{cents: -m.cents, currency: m.currency}, nil
}

// Compare returns -1, 0 or 1 when m is less than, equal to or greater than o.
func (m Money) Compare(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.cents < o.cents:
		return -1, nil
	case m.cents > o.cents:
		return 1, nil
	}
	return 0, nil
}

// Equals reports whether both amount and currency are identical.
func (m Money) Equals(o Money) bool {
	return m.cents == o.cents && m.currency == o.currency
}

// IsNegative reports whether the amount is below zero.
func (m Money) IsNegative() bool { return m.cents < 0 }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.cents == 0 }

// Sign returns -1, 0 or 1 according to the sign of the amount.
func (m Money) Sign() int {
	switch {
	case m.cents < 0:
		return -1
	case m.cents > 0:
		return 1
	}
	return 0
}

// Amount returns the decimal representation with exactly 2 fraction digits.
func (m Money) Amount() string {
	neg := m.cents < 0
	var abs uint64
	if neg {
		abs = uint64(-(m.cents + 1)) + 1
	} else {
		abs = uint64(m.cents)
	}
	s := fmt.Sprintf("%d.%02d", abs/100, abs%100)
	if neg {
		s = "-" + s
	}
	return s
}

// String returns "<amount> <currency>", e.g. "10.50 BRL".
func (m Money) String() string {
	return m.Amount() + " " + m.currency
}

type wireMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON encodes Money as {"amount":"10.50","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := validateCurrency(m.currency); err != nil {
		return nil, err
	}
	return json.Marshal(wireMoney{Amount: m.Amount(), Currency: m.currency})
}

// UnmarshalJSON strictly decodes Money: amount must be a JSON string, unknown
// fields and null are rejected.
func (m *Money) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("%w: null is not a valid money value", ErrInvalidMoney)
	}
	var w wireMoney
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMoney, err)
	}
	parsed, err := ParseMoney(w.Amount, w.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
