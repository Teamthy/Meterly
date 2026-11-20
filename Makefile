.PHONY: test run vet fmt build

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

build:
	go build -o bin/api ./cmd/api

run:
	MODE=dev go run ./cmd/api
