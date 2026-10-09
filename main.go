package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		log.Fatalf("query failed: %v", err)
	}
	fmt.Println(n)
}
