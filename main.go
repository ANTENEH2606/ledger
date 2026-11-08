package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/api"
	"ledger/internal/db"
	"ledger/internal/ledger"
	"ledger/internal/outbox"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is not set")
	}

	// The pool uses its own context, so Ctrl+C does not cut off in-flight postings.
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := api.NewServer(db.New(pool), ledger.NewService(pool))

	var pub outbox.Publisher = outbox.LogPublisher{}
	if target := os.Getenv("WEBHOOK_URL"); target != "" {
		secret := os.Getenv("WEBHOOK_SECRET")
		if len(secret) < 16 {
			return errors.New("WEBHOOK_SECRET must be at least 16 characters when WEBHOOK_URL is set")
		}
		pub = outbox.NewWebhookPublisher(target, secret)
		log.Println("outbox: publishing events to the configured webhook")
	}

	// Background worker: publishes outbox events. It stops before the pool closes.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		outbox.NewWorker(pool, pub, 50).Run(workerCtx, 2*time.Second)
	}()
	defer func() {
		stopWorker()
		<-workerDone
	}()

	httpSrv := &http.Server{
		Addr:              ":8080",
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Cancelled on Ctrl+C (SIGINT) or SIGTERM (what `docker compose stop` sends).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err // the server failed on its own (for example, port already in use)
	case <-ctx.Done():
	}

	log.Println("shutting down: finishing in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Println("stopped cleanly")
	return nil
}
