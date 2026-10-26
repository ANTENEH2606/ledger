package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/api"
	"ledger/internal/db"
	"ledger/internal/ledger"
)

func main() {
	ctx := context.Background()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	q := db.New(pool)
	svc := ledger.NewService(pool)
	srv := api.NewServer(q, svc)

	r := srv.Routes()
	r.GET("/health", func(c *gin.Context) {
		if err := pool.Ping(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	log.Fatal(r.Run(":8080"))
}
