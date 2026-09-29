.PHONY: help build test vet lint fmt check

help:
	@echo "make build  — compile every package"
	@echo "make test   — race-mode unit tests"
	@echo "make vet    — go vet"
	@echo "make lint   — golangci-lint run"
	@echo "make fmt    — gofumpt -w ."
	@echo "make check  — vet + test + lint (what CI runs)"

build:
	go build ./...

test:
	go test -race -timeout 2m ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofumpt -w .

check: vet test lint
