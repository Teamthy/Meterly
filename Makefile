.PHONY: test test-unit vet fmt build migrate run run-pg certify

# Integration tests run when DATABASE_URL is set (see docker-compose.yml).
test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

build:
	go build -o bin/ ./cmd/...

# Live sandbox certification. Skips providers without keys; never runs in CI.
certify:
	go test -tags certify ./internal/certify/...
	go run ./cmd/certify -o certify-report.json

migrate:
	go run ./cmd/migrate

# In-memory store, worker in-process.
run:
	MODE=dev go run ./cmd/api

# PostgreSQL store (run `make migrate` first), worker in-process.
run-pg:
	MODE=dev go run ./cmd/api
