package api

import (
	"errors"
	"net/http"

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
