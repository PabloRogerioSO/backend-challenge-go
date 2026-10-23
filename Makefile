SHELL := bash

-include .env
export

BINARY          := bin/app
COMPOSE_NETWORK := backend-challenge-go_default
MIGRATE_IMAGE   := migrate/migrate:v4.18.1
MIGRATE_DB_URL  := postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable

.PHONY: build run test vet compose-up compose-down migrate-up migrate-down queues

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

migrate-up:
	@mkdir -p migrations
	MSYS_NO_PATHCONV=1 docker run --rm --network $(COMPOSE_NETWORK) -v "$(CURDIR)/migrations:/migrations" $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" up

migrate-down:
	@mkdir -p migrations
	MSYS_NO_PATHCONV=1 docker run --rm --network $(COMPOSE_NETWORK) -v "$(CURDIR)/migrations:/migrations" $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" down 1

queues:
	bash scripts/provision-queues.sh