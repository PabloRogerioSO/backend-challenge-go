SHELL := bash

-include .env
export

BINARY          := bin/app
COMPOSE_NETWORK := backend-challenge-go_default
MIGRATE_IMAGE   := migrate/migrate:v4.18.1
MIGRATE_DB_URL  := postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable
# Git Bash (Windows): "pwd -W" devolve C:/... que o Docker Desktop entende; em Linux/macOS cai no pwd.
MIGRATIONS_DIR  := $(shell pwd -W 2>/dev/null || pwd)/migrations

.PHONY: build run test vet compose-up compose-down migrate-up migrate-down queues check-env

build:
	go build -o $(BINARY) ./cmd/app

run:
	go run ./cmd/app

test:
	go test ./...

vet:
	go vet ./...

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

check-env:
	@test -f .env || { echo "Arquivo .env não encontrado: rode 'cp .env.example .env'" >&2; exit 1; }

migrate-up: check-env
	@mkdir -p migrations
	MSYS_NO_PATHCONV=1 docker run --rm --network $(COMPOSE_NETWORK) -v "$(MIGRATIONS_DIR):/migrations" $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" up

migrate-down: check-env
	@mkdir -p migrations
	MSYS_NO_PATHCONV=1 docker run --rm --network $(COMPOSE_NETWORK) -v "$(MIGRATIONS_DIR):/migrations" $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" down 1

queues:
	bash scripts/provision-queues.sh