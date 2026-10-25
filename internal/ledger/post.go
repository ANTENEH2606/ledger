package ledger

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

type EntryInput struct {
	AccountID int64
	Amount    int64 // positive = debit, negative = credit
}

type PostInput struct {
	IdempotencyKey string
	EffectiveDate  time.Time
	Description    string
	Entries        []EntryInput
}

var (
	ErrTooFewEntries = errors.New("transaction needs at least 2 entries")
	ErrZeroAmount    = errors.New("entry amount must not be zero")
	ErrNoKey         = errors.New("idempotency key is required")
	ErrUnbalanced    = errors.New("entries do not sum to zero")
	ErrOverflow      = errors.New("entry amounts overflow int64")
)

// validate checks a posting request before it touches the database.
// The deferred trigger in Postgres is the real safety net; this gives
// callers a fast, clear error without opening a transaction.
func validate(in PostInput) error {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return ErrNoKey
	}
	if len(in.Entries) < 2 {
		return ErrTooFewEntries
	}

	var sum int64
	for _, e := range in.Entries {
		if e.Amount == 0 {
			return ErrZeroAmount
		}
		next, ok := addChecked(sum, e.Amount)
		if !ok {
			return ErrOverflow
		}
		sum = next
	}

	if sum != 0 {
		return ErrUnbalanced
	}
	return nil
}

// addChecked returns a+b, and false if the result would overflow int64.
func addChecked(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

// requestHash fingerprints what the client asked for: the entries and the
// effective date. It leaves out Description and anything the server generates.
func requestHash(in PostInput) string {
	// Copy so we never reorder the caller's slice.
	entries := slices.Clone(in.Entries)

	// Same entries in any order must give the same hash.
	// an order-independent hash is the whole point, so [A,B] and [B,A] must match
	slices.SortFunc(entries, func(a, b EntryInput) int {
		if c := cmp.Compare(a.AccountID, b.AccountID); c != 0 {
			return c
		}
		return cmp.Compare(a.Amount, b.Amount)
	})

	var b strings.Builder
	b.WriteString("ledger/request/v1|")                  // version tag (domain separation)
	b.WriteString(in.EffectiveDate.Format("2006-01-02")) // this keeps only the date. A different time of day on the same date gives the same hash
	for _, e := range entries {
		fmt.Fprintf(&b, "|%d:%d", e.AccountID, e.Amount) // the | and : separators keep account 1, amount 25 apart from account 12, amount 5
	}
	text := b.String()
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
