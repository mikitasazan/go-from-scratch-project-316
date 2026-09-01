BINARY = bin/hexlet-go-crawler

build:
	go build -o $(BINARY) ./cmd/hexlet-go-crawler

run:
	go run ./cmd/hexlet-go-crawler $(URL)

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

test:
	go test ./...

.PHONY: build run lint lint-fix test
