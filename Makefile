.PHONY: run build test vet fmt

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test ./... -v

vet:
	go vet ./...

fmt:
	gofmt -l -w .
