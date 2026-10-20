package ledger

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAccountIDs(t *testing.T) {
	entries := []EntryInput{
		{AccountID: 5, Amount: 10},
		{AccountID: 2, Amount: -4},
		{AccountID: 5, Amount: -6},
		{AccountID: 9, Amount: 0},
	}
	got := accountIDs(entries)
	want := []int64{2, 5, 9}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOverdrawn(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		balance     int64
		want        bool
	}{
		{"asset in the black", "asset", 500, false},
		{"asset at zero", "asset", 0, false},
		{"asset below zero", "asset", -1, true},
		{"expense below zero", "expense", -1, true},
		{"liability holding funds (credit balance)", "liability", -1000, false},
		{"liability at zero", "liability", 0, false},
		{"liability on the debit side", "liability", 1, true},
		{"equity on the debit side", "equity", 1, true},
		{"revenue on the debit side", "revenue", 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := overdrawn(tc.accountType, tc.balance); got != tc.want {
				t.Fatalf("overdrawn(%q, %d) = %v, want %v", tc.accountType, tc.balance, got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"wrapped deadlock", fmt.Errorf("commit: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
