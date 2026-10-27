package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ledger/internal/db"
	"ledger/internal/ledger"
)

type Server struct {
	q   *db.Queries
	svc *ledger.Service
}

func NewServer(q *db.Queries, svc *ledger.Service) *Server {
	return &Server{q: q, svc: svc}
}

func (s *Server) Routes() *gin.Engine {
	r := gin.Default()
	r.SetTrustedProxies(nil)

	r.POST("/accounts", s.createAccount)
	r.POST("/transactions", s.postTransaction)
	r.GET("/accounts/:id/statement", s.getStatement)
	r.GET("/accounts/:id/balance", s.getBalance)
	r.GET("/trial-balance", s.getTrialBalance)
	return r
}

type createAccountRequest struct {
	Code     string `json:"code" binding:"required"`
	Name     string `json:"name" binding:"required"`
	Type     string `json:"type" binding:"required,oneof=asset liability equity revenue expense"`
	Currency string `json:"currency" binding:"required,len=3"`
}

func (s *Server) createAccount(c *gin.Context) {
	var req createAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	acc, err := s.q.CreateAccount(c.Request.Context(), db.CreateAccountParams{
		Code:     req.Code,
		Name:     req.Name,
		Type:     req.Type,
		Currency: req.Currency,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "an account with this code already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create account"})
		return
	}

	c.JSON(http.StatusCreated, acc)
}

type entryRequest struct {
	AccountID int64 `json:"account_id" binding:"required"`
	Amount    int64 `json:"amount" binding:"required"`
}

type postTransactionRequest struct {
	EffectiveDate string         `json:"effective_date" binding:"required"` // YYYY-MM-DD
	Description   string         `json:"description"`
	Entries       []entryRequest `json:"entries" binding:"required,min=2,dive"`
}

func (s *Server) postTransaction(c *gin.Context) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header is required"})
		return
	}

	var req postTransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	date, err := time.Parse("2006-01-02", req.EffectiveDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "effective_date must be YYYY-MM-DD"})
		return
	}

	entries := make([]ledger.EntryInput, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = ledger.EntryInput{AccountID: e.AccountID, Amount: e.Amount}
	}

	tx, err := s.svc.PostTransaction(c.Request.Context(), ledger.PostInput{
		IdempotencyKey: key,
		EffectiveDate:  date,
		Description:    req.Description,
		Entries:        entries,
	})
	if err != nil {
		status, msg := mapError(err)
		c.JSON(status, gin.H{"error": msg})
		return
	}

	c.JSON(http.StatusCreated, tx)
}

func mapError(err error) (int, string) {
	switch {
	case errors.Is(err, ledger.ErrTooFewEntries),
		errors.Is(err, ledger.ErrZeroAmount),
		errors.Is(err, ledger.ErrNoKey),
		errors.Is(err, ledger.ErrUnbalanced),
		errors.Is(err, ledger.ErrOverflow):
		return http.StatusBadRequest, err.Error()

	case errors.Is(err, ledger.ErrUnknownAccount),
		errors.Is(err, ledger.ErrMixedCurrency),
		errors.Is(err, ledger.ErrInsufficientFunds):
		return http.StatusUnprocessableEntity, err.Error()

	case errors.Is(err, ledger.ErrIdempotencyKeyReuse):
		return http.StatusConflict, err.Error()

	default:
		return http.StatusInternalServerError, "internal error"
	}
}

func (s *Server) getStatement(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	ctx := c.Request.Context()

	acc, err := s.q.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	rows, err := s.q.GetAccountStatement(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	if rows == nil {
		rows = []db.GetAccountStatementRow{} // so JSON shows [] instead of null
	}

	c.JSON(http.StatusOK, gin.H{
		"account":   acc,
		"statement": rows,
	})
}

// parseAsOf reads ?as_of=YYYY-MM-DD; empty means today.
func parseAsOf(c *gin.Context) (time.Time, error) {
	v := c.Query("as_of")
	if v == "" {
		return time.Now(), nil
	}
	return time.Parse("2006-01-02", v)
}

type currencyTotals struct {
	Currency     string `json:"currency"`
	TotalDebits  int64  `json:"total_debits"`
	TotalCredits int64  `json:"total_credits"`
	Balanced     bool   `json:"balanced"`
}

func (s *Server) getTrialBalance(c *gin.Context) {
	asOf, err := parseAsOf(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "as_of must be YYYY-MM-DD"})
		return
	}

	rows, err := s.q.GetTrialBalance(c.Request.Context(), asOf)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// Totals per currency, computed from the very rows we return,
	// so the totals always match what the client sees.
	byCurrency := make(map[string]*currencyTotals)
	for _, r := range rows {
		t, ok := byCurrency[r.Currency]
		if !ok {
			t = &currencyTotals{Currency: r.Currency}
			byCurrency[r.Currency] = t
		}
		t.TotalDebits += r.TotalDebits
		t.TotalCredits += r.TotalCredits
	}

	totals := make([]currencyTotals, 0, len(byCurrency))
	for _, t := range byCurrency {
		t.Balanced = t.TotalDebits == t.TotalCredits
		totals = append(totals, *t)
	}
	slices.SortFunc(totals, func(a, b currencyTotals) int {
		return strings.Compare(a.Currency, b.Currency)
	})

	if rows == nil {
		rows = []db.GetTrialBalanceRow{}
	}

	c.JSON(http.StatusOK, gin.H{
		"as_of":    asOf.Format("2006-01-02"),
		"accounts": rows,
		"totals":   totals,
	})
}

func (s *Server) getBalance(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return
	}

	asOf, err := parseAsOf(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "as_of must be YYYY-MM-DD"})
		return
	}

	ctx := c.Request.Context()

	acc, err := s.q.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	balance, err := s.q.GetAccountBalanceAsOf(ctx, db.GetAccountBalanceAsOfParams{
		AccountID: id,
		AsOf:      asOf,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"account_id": acc.ID,
		"currency":   acc.Currency,
		"as_of":      asOf.Format("2006-01-02"),
		"balance":    balance, // minor units; positive = net debit, negative = net credit
	})
}
