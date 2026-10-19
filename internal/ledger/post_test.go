package ledger

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		in      PostInput
		wantErr error
	}{
		{
			name: "1. valid: balanced pair",
			in: PostInput{
				IdempotencyKey: "k1",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
					{AccountID: 2, Amount: -100},
				},
			},
			wantErr: nil,
		},
		{
			name: "2. only one entry",
			in: PostInput{
				IdempotencyKey: "k2",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
				},
			},
			wantErr: ErrTooFewEntries,
		},
		{
			name: "3. an entry with amount zero",
			in: PostInput{
				IdempotencyKey: "k3",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
					{AccountID: 2, Amount: 0},
				},
			},
			wantErr: ErrZeroAmount,
		},
		{
			name: "4. unbalanced",
			in: PostInput{
				IdempotencyKey: "k4",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
					{AccountID: 2, Amount: -90},
				},
			},
			wantErr: ErrUnbalanced,
		},
		{
			name: "5. empty idempotency key",
			in: PostInput{
				IdempotencyKey: "",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
					{AccountID: 2, Amount: -100},
				},
			},
			wantErr: ErrNoKey,
		},
		// Extra cases
		{
			name: "6. valid: one debit split across two credits",
			in: PostInput{
				IdempotencyKey: "k6",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: 100},
					{AccountID: 2, Amount: -60},
					{AccountID: 3, Amount: -40},
				},
			},
			wantErr: nil,
		},
		{
			name: "7. sum would overflow int64",
			in: PostInput{
				IdempotencyKey: "k7",
				EffectiveDate:  now,
				Entries: []EntryInput{
					{AccountID: 1, Amount: math.MaxInt64},
					{AccountID: 2, Amount: 1},
					{AccountID: 3, Amount: -5},
				},
			},
			wantErr: ErrOverflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}
