package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
	"ledger/internal/ledger"
)

// testRouter builds the real router on the real database.
// Skipped unless DATABASE_URL is set, like the other integration tests.
func testRouter(t *testing.T) *gin.Engine {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	gin.SetMode(gin.TestMode)
	return NewServer(db.New(pool), ledger.NewService(pool)).Routes()
}

// do sends one request to the router and returns the recorded response.
func do(r http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateAccount_DuplicateCode(t *testing.T) {
	r := testRouter(t)

	code := fmt.Sprintf("API%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"code":%q,"name":"Cash","type":"asset","currency":"ETB"}`, code)

	if w := do(r, "POST", "/accounts", body, nil); w.Code != http.StatusCreated {
		t.Fatalf("first create: got %d, want 201: %s", w.Code, w.Body)
	}
	if w := do(r, "POST", "/accounts", body, nil); w.Code != http.StatusConflict {
		t.Fatalf("duplicate create: got %d, want 409: %s", w.Code, w.Body)
	}
}

// newAPIAccount creates an account through the API and returns its id.
func newAPIAccount(t *testing.T, r http.Handler) int64 {
	t.Helper()

	code := fmt.Sprintf("API%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"code":%q,"name":"Test","type":"asset","currency":"ETB"}`, code)

	w := do(r, "POST", "/accounts", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create account: got %d, want 201: %s", w.Code, w.Body)
	}

	var acc struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	return acc.ID
}

// txBody builds a two-entry transaction request: amount debit on a, credit on b (the two amounts must sum to zero).
func txBody(a, b, debit, credit int64) string {
	return fmt.Sprintf(
		`{"effective_date":"2026-10-27","description":"api test","entries":[{"account_id":%d,"amount":%d},{"account_id":%d,"amount":%d}]}`,
		a, debit, b, credit)
}

func uniqueKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestPostTransaction_MissingKey(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	w := do(r, "POST", "/transactions", txBody(a, b, 10000, -10000), nil) // no header
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body)
	}
}

func TestPostTransaction_Unbalanced(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	h := map[string]string{"Idempotency-Key": uniqueKey("unbal")}
	w := do(r, "POST", "/transactions", txBody(a, b, 10000, -9000), h)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body)
	}
}

func TestPostTransaction_KeyReuse(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	h := map[string]string{"Idempotency-Key": uniqueKey("reuse")}

	if w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h); w.Code != http.StatusCreated {
		t.Fatalf("first post: got %d, want 201: %s", w.Code, w.Body)
	}
	if w := do(r, "POST", "/transactions", txBody(a, b, 500, -500), h); w.Code != http.StatusConflict {
		t.Fatalf("reuse with different amount: got %d, want 409: %s", w.Code, w.Body)
	}
	// A genuine retry of the original request still succeeds.
	if w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h); w.Code != http.StatusCreated {
		t.Fatalf("retry: got %d, want 201: %s", w.Code, w.Body)
	}
}

func TestStatement_NotFound(t *testing.T) {
	r := testRouter(t)

	w := do(r, "GET", "/accounts/999999999/statement", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", w.Code, w.Body)
	}
}

func TestTrialBalance_Balanced(t *testing.T) {
	r := testRouter(t)

	w := do(r, "GET", "/trial-balance", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	// Decode only the totals; the account list can be huge.
	var resp struct {
		Totals []struct {
			Currency     string `json:"currency"`
			TotalDebits  int64  `json:"total_debits"`
			TotalCredits int64  `json:"total_credits"`
			Balanced     bool   `json:"balanced"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, tot := range resp.Totals {
		if tot.TotalDebits != tot.TotalCredits {
			t.Errorf("%s: debits %d != credits %d", tot.Currency, tot.TotalDebits, tot.TotalCredits)
		}
		if !tot.Balanced {
			t.Errorf("%s: balanced = false", tot.Currency)
		}
	}
}

func TestTrialBalance_BadDate(t *testing.T) {
	r := testRouter(t)

	w := do(r, "GET", "/trial-balance?as_of=nope", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body)
	}
}

// balanceOf reads an account's balance through the API.
func balanceOf(t *testing.T, r http.Handler, id int64, asOf string) int64 {
	t.Helper()

	path := fmt.Sprintf("/accounts/%d/balance?as_of=%s", id, asOf)
	w := do(r, "GET", path, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balance of %d: got %d, want 200: %s", id, w.Code, w.Body)
	}

	var resp struct {
		Balance int64 `json:"balance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	return resp.Balance
}

func TestBalance_AfterPosting(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	h := map[string]string{"Idempotency-Key": uniqueKey("bal")}
	if w := do(r, "POST", "/transactions", txBody(a, b, 10000, -10000), h); w.Code != http.StatusCreated {
		t.Fatalf("post: got %d, want 201: %s", w.Code, w.Body)
	}

	// txBody posts on 2026-10-27, so that date includes the transaction.
	if got := balanceOf(t, r, a, "2026-10-27"); got != 10000 {
		t.Errorf("balance of a = %d, want 10000", got)
	}
	if got := balanceOf(t, r, b, "2026-10-27"); got != -10000 {
		t.Errorf("balance of b = %d, want -10000", got)
	}

	// The day before the effective date, nothing had happened yet.
	if got := balanceOf(t, r, a, "2026-10-26"); got != 0 {
		t.Errorf("balance of a on 2026-10-26 = %d, want 0", got)
	}
}
