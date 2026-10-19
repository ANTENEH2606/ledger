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
