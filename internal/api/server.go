package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
