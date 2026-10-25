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

func TestRequestHash(t *testing.T) {
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)

	base := PostInput{
		IdempotencyKey: "k1",
		EffectiveDate:  day,
		Description:    "rent",
		Entries: []EntryInput{
			{AccountID: 1, Amount: -100},
			{AccountID: 2, Amount: 100},
		},
	}

	// with returns a copy of base after applying a change.
	with := func(change func(in *PostInput)) PostInput {
		in := base
		in.Entries = append([]EntryInput(nil), base.Entries...)
		change(&in)
		return in
	}

	tests := []struct {
		name  string
		other PostInput
		same  bool // true: hash must equal base's; false: must differ
	}{
		{"identical input", with(func(in *PostInput) {}), true},
		{"entries in other order", with(func(in *PostInput) {
			in.Entries[0], in.Entries[1] = in.Entries[1], in.Entries[0]
		}), true},
		{"different description", with(func(in *PostInput) { in.Description = "something else" }), true},
		{"different time of day, same date", with(func(in *PostInput) {
			in.EffectiveDate = day.Add(15 * time.Hour)
		}), true},
		{"different amount", with(func(in *PostInput) {
			in.Entries[0].Amount, in.Entries[1].Amount = -500, 500
		}), false},
		{"different account", with(func(in *PostInput) { in.Entries[1].AccountID = 3 }), false},
		{"different date", with(func(in *PostInput) { in.EffectiveDate = day.AddDate(0, 0, 1) }), false},
		{"boundary shift (1:25 vs 12:5)", PostInput{
			EffectiveDate: day,
			Entries:       []EntryInput{{AccountID: 12, Amount: 5}, {AccountID: 1, Amount: -5}},
		}, false},
	}

	want := requestHash(base)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestHash(tc.other)
			if tc.same && got != want {
				t.Errorf("hash changed but should be the same")
			}
			if !tc.same && got == want {
				t.Errorf("hash is the same but should differ")
			}
		})
	}

	// It must not reorder the caller's slice.
	in := with(func(in *PostInput) {
		in.Entries[0], in.Entries[1] = in.Entries[1], in.Entries[0]
	})
	first := in.Entries[0]
	requestHash(in)
	if in.Entries[0] != first {
		t.Errorf("requestHash reordered the caller's entries")
	}
}
