package outbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"ledger/internal/db"
)

func TestBackoff(t *testing.T) {
	cases := map[int32]time.Duration{
		0: 5 * time.Second, 1: 10 * time.Second, 2: 20 * time.Second,
		7: 10 * time.Minute, 100: 10 * time.Minute,
	}
	for attempts, want := range cases {
		if got := Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

type received struct {
	body []byte
	hdr  http.Header
}

func TestWebhook_SignatureVerifies(t *testing.T) {
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- received{b, r.Header.Clone()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	secret := "test-secret-0123456789"
	ev := db.OutboxEvent{ID: 7, EventType: "transaction.posted", Payload: []byte(`{"transaction_id": 1}`)}
	if err := NewWebhookPublisher(srv.URL, secret).Publish(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	r := <-got
	if string(r.body) != string(ev.Payload) {
		t.Fatalf("body = %q, want %q", r.body, ev.Payload)
	}
	if r.hdr.Get("X-Ledger-Event-Id") != strconv.Itoa(7) {
		t.Errorf("event id header = %q", r.hdr.Get("X-Ledger-Event-Id"))
	}

	ts, sig, now := r.hdr.Get("X-Ledger-Timestamp"), r.hdr.Get("X-Ledger-Signature"), time.Now()
	if !VerifySignature([]byte(secret), ts, sig, r.body, 5*time.Minute, now) {
		t.Fatal("a genuine request failed verification")
	}
	if VerifySignature([]byte(secret), ts, sig, append(r.body, ' '), 5*time.Minute, now) {
		t.Error("a tampered body passed verification")
	}
	if VerifySignature([]byte("some-other-secret-0000"), ts, sig, r.body, 5*time.Minute, now) {
		t.Error("the wrong secret passed verification")
	}
	if VerifySignature([]byte(secret), ts, sig, r.body, 5*time.Minute, now.Add(time.Hour)) {
		t.Error("an hour-old request passed verification")
	}
}

func TestWebhook_OnlyTwoXXIsSuccess(t *testing.T) {
	cases := map[int]bool{200: true, 204: true, 302: false, 400: false, 500: false}
	for status, wantOK := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == 302 {
				w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			}
			w.WriteHeader(status)
		}))
		err := NewWebhookPublisher(srv.URL, "test-secret-0123456789").
			Publish(context.Background(), db.OutboxEvent{ID: 1, Payload: []byte(`{}`)})
		srv.Close()
		if (err == nil) != wantOK {
			t.Errorf("status %d: err = %v, want success = %v", status, err, wantOK)
		}
	}
}
