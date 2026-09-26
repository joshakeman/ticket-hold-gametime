DATABASE_URL ?= postgres://tickets:tickets@localhost:5433/tickets?sslmode=disable

.PHONY: db test run seed

db:
	docker compose up -d --wait db

test: db
	TEST_DATABASE_URL='$(DATABASE_URL)' go test -race -count=1 ./...

run: db
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/server

# Loads one demo event with 20 seats (run the server once first so migrations have applied).
seed:
	docker compose exec -T db psql -U tickets -d tickets < scripts/seed.sql
