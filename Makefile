# File: Makefile
# Project: LLM Cost Intelligence Gateway

.PHONY: build run test test-cover bench lint docker-up docker-down migrate clean test-integration load-test seed benchmark-report

BINARY=gateway
BUILD_FLAGS=-ldflags="-s -w" -trimpath

build:
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o bin/$(BINARY) ./cmd/gateway

run:
	go run ./cmd/gateway

test:
	go test ./... -race -count=1 -timeout=120s

test-cover:
	go test ./... -race -coverprofile=coverage.out -covermode=atomic
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | grep total

bench:
	go test -bench=. -benchtime=10s -benchmem -run=^$$ ./tests/benchmarks/

lint:
	golangci-lint run ./...

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down -v

migrate:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

test-integration:
	go test ./tests/integration/... -tags=integration -v -timeout=180s

load-test:
	k6 run tests/load/k6_load_test.js

seed:
	python3 scripts/seed_demo_data.py

benchmark-report:
	python3 scripts/benchmark_report.py

clean:
	rm -rf bin/ coverage.out coverage.html

# Default target
all: build
