package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ledger/internal/db"
)

// WebhookPublisher POSTs each event to one URL, signed with a shared secret.
type WebhookPublisher struct {
	target string
	secret []byte
	client *http.Client
}

func NewWebhookPublisher(target, secret string) *WebhookPublisher {
	return &WebhookPublisher{
		target: target,
		secret: []byte(secret),
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A redirect would send the signed body somewhere else. Treat it as a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// sign returns "v1=" + hex(HMAC-SHA256(secret, "<timestamp>.<body>")).
// The timestamp is part of what is signed, so an old request cannot be replayed later.
func sign(secret []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (p *WebhookPublisher) Publish(ctx context.Context, ev db.OutboxEvent) error {
	ts := time.Now().Unix()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.target, bytes.NewReader(ev.Payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Event-Id", strconv.FormatInt(ev.ID, 10)) // receivers dedupe on this
	req.Header.Set("X-Ledger-Event-Type", ev.EventType)
	req.Header.Set("X-Ledger-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Ledger-Signature", sign(p.secret, ts, ev.Payload))

	resp, err := p.client.Do(req)
	if err != nil {
		// url.Error contains the URL, which may hold a token. It must not end up in last_error.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("send: %w", ue.Err)
		}
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// VerifySignature is what a receiver does. It also documents the scheme.
// hmac.Equal compares in constant time, so the check does not leak how many bytes matched.
func VerifySignature(secret []byte, timestamp, signature string, body []byte, maxAge time.Duration, now time.Time) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(ts, 0))
	if age > maxAge || age < -maxAge {
		return false
	}
	return hmac.Equal([]byte(sign(secret, ts, body)), []byte(signature))
}
