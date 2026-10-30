.RECIPEPREFIX = >
DB_URL ?= $(shell grep '^DATABASE_URL=' .env | cut -d= -f2-)
TEST_DB_URL ?= $(shell grep '^TEST_DATABASE_URL=' .env | cut -d= -f2-)

.PHONY: run build tidy db-up migrate-up migrate-down migrate-new migrate-status psql test test-db test-db-migrate up down test-db-create

run:
> @DATABASE_URL="$(DB_URL)"  go run main.go

build:
> @DATABASE_URL="$(DB_URL)"  go build -o ledger .

tidy:
> go mod tidy

db-up:
> @docker start ledger >/dev/null 2>&1 || true
> @docker ps --filter name=ledger --format '{{.Status}}' | grep -q '^Up' || (echo "error: database is not running. Start it with: docker compose up -d db" && exit 1)
migrate-up: db-up
> @migrate -path db/migrations -database "$(DB_URL)" up

migrate-down: db-up
> @migrate -path db/migrations -database "$(DB_URL)" down 1

migrate-status: db-up
> @migrate -path db/migrations -database "$(DB_URL)" version

migrate-new:
> @read -p "migration name: " name; \
> migrate create -ext sql -dir db/migrations -seq $$name

psql: db-up
> docker exec -it ledger psql -U ledger -d ledger

test:
> go test ./...

test-db-migrate: db-up
> @migrate -path db/migrations -database "$(TEST_DB_URL)" up

test-db: db-up
> @DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

test-db-create: db-up
> @docker exec ledger psql -U ledger -d postgres -c "CREATE DATABASE ledger_test"

up:
> docker compose up -d --build

down:
> docker compose down