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
	// A genuine retry of the original request returns the original, with 200 (not 201).
	if w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h); w.Code != http.StatusOK {
		t.Fatalf("retry: got %d, want 200: %s", w.Code, w.Body)
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

// newWalletID creates an asset account that forbids overdraft, through the API.
func newWalletID(t *testing.T, r http.Handler) int64 {
	t.Helper()

	code := fmt.Sprintf("WAL%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"code":%q,"name":"Wallet","type":"asset","currency":"ETB","allow_overdraft":false}`, code)

	w := do(r, "POST", "/accounts", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create wallet: got %d, want 201: %s", w.Code, w.Body)
	}
	var acc struct {
		ID             int64 `json:"id"`
		AllowOverdraft bool  `json:"allow_overdraft"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}
	if acc.AllowOverdraft {
		t.Fatalf("wallet was created with allow_overdraft=true, want false")
	}
	return acc.ID
}

// Leaving allow_overdraft out must keep the old behaviour: overdraft allowed.
func TestCreateAccount_OverdraftDefaultsToAllowed(t *testing.T) {
	r := testRouter(t)

	code := fmt.Sprintf("API%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"code":%q,"name":"Default","type":"asset","currency":"ETB"}`, code)

	w := do(r, "POST", "/accounts", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body)
	}
	var acc struct {
		AllowOverdraft bool `json:"allow_overdraft"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if !acc.AllowOverdraft {
		t.Errorf("allow_overdraft = false when the field was omitted, want true")
	}
}

// A wallet created with allow_overdraft=false cannot be overdrawn through the API.
func TestPostTransaction_NoOverdraftWallet(t *testing.T) {
	r := testRouter(t)
	wallet := newWalletID(t, r)
	other := newAPIAccount(t, r)

	// Deposit 100: debit the wallet, credit the other account. Allowed.
	h := map[string]string{"Idempotency-Key": uniqueKey("dep")}
	if w := do(r, "POST", "/transactions", txBody(wallet, other, 100, -100), h); w.Code != http.StatusCreated {
		t.Fatalf("deposit: got %d, want 201: %s", w.Code, w.Body)
	}

	// Withdraw 150 (only 100 inside): credit the wallet. Must be refused.
	h = map[string]string{"Idempotency-Key": uniqueKey("wd")}
	w := do(r, "POST", "/transactions", txBody(other, wallet, 150, -150), h)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("overdraw: got %d, want 422: %s", w.Code, w.Body)
	}
}

func TestPostTransaction_ReplayReturns200(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	h := map[string]string{"Idempotency-Key": uniqueKey("replay")}
	body := txBody(a, b, 100, -100)

	first := do(r, "POST", "/transactions", body, h)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: got %d, want 201: %s", first.Code, first.Body)
	}
	second := do(r, "POST", "/transactions", body, h) // same key, same body: a network retry
	if second.Code != http.StatusOK {
		t.Fatalf("replay: got %d, want 200: %s", second.Code, second.Body)
	}

	var f, s struct {
		ID       int64 `json:"id"`
		Replayed bool  `json:"replayed"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if f.Replayed {
		t.Errorf("first response has replayed=true, want false")
	}
	if !s.Replayed {
		t.Errorf("replay response has replayed=false, want true")
	}
	if f.ID != s.ID {
		t.Errorf("replay returned id %d, want the original id %d", s.ID, f.ID)
	}
}

func TestPostTransaction_ResponseHidesRequestHash(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	h := map[string]string{"Idempotency-Key": uniqueKey("hide")}
	w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "request_hash") {
		t.Errorf("response exposes request_hash: %s", w.Body)
	}
}

func TestHealth(t *testing.T) {
	r := testRouter(t)
	w := do(r, "GET", "/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("unexpected body: %s", w.Body)
	}
}

func TestStatement_Pagination(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)

	for i := 0; i < 5; i++ {
		h := map[string]string{"Idempotency-Key": uniqueKey(fmt.Sprintf("pg%d", i))}
		if w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h); w.Code != http.StatusCreated {
			t.Fatalf("post %d: got %d: %s", i, w.Code, w.Body)
		}
	}

	type page struct {
		Statement []struct {
			ID             int64 `json:"id"`
			RunningBalance int64 `json:"running_balance"`
		} `json:"statement"`
		NextCursor *string `json:"next_cursor"`
	}

	var seen []int64
	var lastBalance int64
	path := fmt.Sprintf("/accounts/%d/statement?limit=2", a)
	for i := 0; i < 10; i++ {
		w := do(r, "GET", path, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d: %s", w.Code, w.Body)
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		for _, row := range p.Statement {
			seen = append(seen, row.ID)
			lastBalance = row.RunningBalance
		}
		if p.NextCursor == nil {
			break
		}
		path = fmt.Sprintf("/accounts/%d/statement?limit=2&cursor=%s", a, *p.NextCursor)
	}

	if len(seen) != 5 {
		t.Fatalf("saw %d entries over all pages, want 5: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("entries not strictly increasing: %v", seen)
		}
	}
	if lastBalance != 500 {
		t.Errorf("running balance on the last page = %d, want 500", lastBalance)
	}
}

func TestStatement_BadPagingParams(t *testing.T) {
	r := testRouter(t)
	a := newAPIAccount(t, r)

	for _, q := range []string{"limit=0", "limit=1000", "limit=abc", "cursor=garbage", "cursor=2026-13-45_1"} {
		w := do(r, "GET", fmt.Sprintf("/accounts/%d/statement?%s", a, q), "", nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", q, w.Code, w.Body)
		}
	}
}

// postTx posts a 100 ETB transfer from a to b and returns the new transaction id.
func postTx(t *testing.T, r http.Handler, a, b int64) int64 {
	t.Helper()

	h := map[string]string{"Idempotency-Key": uniqueKey("orig")}
	w := do(r, "POST", "/transactions", txBody(a, b, 100, -100), h)
	if w.Code != http.StatusCreated {
		t.Fatalf("post: got %d, want 201: %s", w.Code, w.Body)
	}
	var tx struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tx); err != nil {
		t.Fatalf("decode transaction: %v", err)
	}
	return tx.ID
}

const reverseBody = `{"effective_date":"2026-10-27"}`

func reversePath(id int64) string {
	return fmt.Sprintf("/transactions/%d/reverse", id)
}

func TestReverseAPI_NetsToZero(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)
	orig := postTx(t, r, a, b)

	h := map[string]string{"Idempotency-Key": uniqueKey("rev")}
	w := do(r, "POST", reversePath(orig), reverseBody, h)
	if w.Code != http.StatusCreated {
		t.Fatalf("reverse: got %d, want 201: %s", w.Code, w.Body)
	}

	var rev struct {
		ID                    int64  `json:"id"`
		ReversesTransactionID *int64 `json:"reverses_transaction_id"`
		Replayed              bool   `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rev); err != nil {
		t.Fatal(err)
	}
	if rev.ReversesTransactionID == nil || *rev.ReversesTransactionID != orig {
		t.Errorf("reverses_transaction_id = %v, want %d", rev.ReversesTransactionID, orig)
	}
	if rev.Replayed {
		t.Errorf("first reversal has replayed=true, want false")
	}

	if got := balanceOf(t, r, a, "2026-10-27"); got != 0 {
		t.Errorf("balance of a = %d, want 0", got)
	}
	if got := balanceOf(t, r, b, "2026-10-27"); got != 0 {
		t.Errorf("balance of b = %d, want 0", got)
	}
}

func TestReverseAPI_OnlyOnce(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)
	orig := postTx(t, r, a, b)

	h1 := map[string]string{"Idempotency-Key": uniqueKey("rev1")}
	if w := do(r, "POST", reversePath(orig), reverseBody, h1); w.Code != http.StatusCreated {
		t.Fatalf("first reversal: got %d, want 201: %s", w.Code, w.Body)
	}
	h2 := map[string]string{"Idempotency-Key": uniqueKey("rev2")}
	if w := do(r, "POST", reversePath(orig), reverseBody, h2); w.Code != http.StatusConflict {
		t.Fatalf("second reversal: got %d, want 409: %s", w.Code, w.Body)
	}
}

func TestReverseAPI_UnknownTransaction(t *testing.T) {
	r := testRouter(t)

	h := map[string]string{"Idempotency-Key": uniqueKey("revnone")}
	w := do(r, "POST", reversePath(999999999), reverseBody, h)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", w.Code, w.Body)
	}
}

func TestReverseAPI_CannotReverseAReversal(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)
	orig := postTx(t, r, a, b)

	h := map[string]string{"Idempotency-Key": uniqueKey("rev")}
	w := do(r, "POST", reversePath(orig), reverseBody, h)
	if w.Code != http.StatusCreated {
		t.Fatalf("reverse: got %d, want 201: %s", w.Code, w.Body)
	}
	var rev struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rev); err != nil {
		t.Fatal(err)
	}

	h2 := map[string]string{"Idempotency-Key": uniqueKey("revrev")}
	if w := do(r, "POST", reversePath(rev.ID), reverseBody, h2); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reverse a reversal: got %d, want 422: %s", w.Code, w.Body)
	}
}

func TestReverseAPI_RetryIsAReplay(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)
	orig := postTx(t, r, a, b)

	h := map[string]string{"Idempotency-Key": uniqueKey("revretry")}
	first := do(r, "POST", reversePath(orig), reverseBody, h)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: got %d, want 201: %s", first.Code, first.Body)
	}
	second := do(r, "POST", reversePath(orig), reverseBody, h) // same key, same body: a network retry
	if second.Code != http.StatusOK {
		t.Fatalf("retry: got %d, want 200: %s", second.Code, second.Body)
	}

	var f, s struct {
		ID       int64 `json:"id"`
		Replayed bool  `json:"replayed"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Replayed || s.ID != f.ID {
		t.Errorf("retry: id %d replayed=%v, want id %d replayed=true", s.ID, s.Replayed, f.ID)
	}
}

func TestReverseAPI_BadRequests(t *testing.T) {
	r := testRouter(t)
	a, b := newAPIAccount(t, r), newAPIAccount(t, r)
	orig := postTx(t, r, a, b)
	key := map[string]string{"Idempotency-Key": uniqueKey("revbad")}

	cases := []struct {
		name    string
		path    string
		body    string
		headers map[string]string
	}{
		{"missing key", reversePath(orig), reverseBody, nil},
		{"missing date", reversePath(orig), `{}`, key},
		{"bad date", reversePath(orig), `{"effective_date":"27/10/2026"}`, key},
		{"bad id", "/transactions/abc/reverse", reverseBody, key},
		{"zero id", "/transactions/0/reverse", reverseBody, key},
	}
	for _, c := range cases {
		w := do(r, "POST", c.path, c.body, c.headers)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", c.name, w.Code, w.Body)
		}
	}

	// None of those bad requests may have reversed anything.
	if got := balanceOf(t, r, a, "2026-10-27"); got != 100 {
		t.Errorf("balance of a = %d after bad requests, want 100", got)
	}
}
