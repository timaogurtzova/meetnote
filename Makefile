APP := meetnote
BUILD_VERSION ?= dev
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILD_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.buildVersion=$(BUILD_VERSION) -X main.buildDate=$(BUILD_DATE) -X main.buildCommit=$(BUILD_COMMIT)
TEST_DATABASE_URL ?= postgres://meetnote:meetnote@localhost:55432/meetnote?sslmode=disable

.PHONY: build test test-race vet lint check db-up integration bot worker up down logs

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/meetnote

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint:
	go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

check: vet lint test-race

db-up:
	docker compose up -d --wait postgres

integration: db-up
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./internal/postgres

bot:
	go run ./cmd/meetnote bot

worker:
	go run ./cmd/meetnote worker

up:
	docker compose up -d --build --wait

down:
	docker compose down

logs:
	docker compose logs -f worker
