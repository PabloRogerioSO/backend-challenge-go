# Backend Challenge — Distributed Wagering Processing in Go

This project implements a financial processing service for wagering, built to withstand high concurrency, network failures, and ensure transactional consistency, persistent idempotency, and ledger integrity.

## 🛠️ Prerequisites

Before you begin, ensure you have:
- **Go** (version 1.23 or higher)
- **Docker** and **Docker Compose** (Docker Desktop on Windows/Mac)
- **Git Bash** (recommended for Windows users, as the shell commands are bash-based)

## ⚙️ Environment Variables

The project uses environment variables for configuration. Before starting the services, create the `.env` file from the example:

```bash
cp .env.example .env
```

## 🚀 Running the Infrastructure

The local environment spins up PostgreSQL (database), Keycloak (authentication), and LocalStack (SQS emulator) via Docker Compose.

```bash
docker compose up -d
```

Verify that the services are healthy:
```bash
docker compose ps
```
*(PostgreSQL should be `healthy` and the other services `Up`).*

## 🗄️ Database and Migrations

To create the database structure (tables, constraints, immutability triggers), run the migrations.

**Option 1: Using Make (Mac/Linux)**
```bash
make migrate-up
make migrate-down
make migrate-up
```

**Option 2: Using Docker directly (Windows / without Make)**
```bash
MSYS_NO_PATHCONV=1 docker run --rm --network backend-challenge-go_default -v "$(pwd)/migrations:/migrations" migrate/migrate:v4.18.1 -path=/migrations -database "postgres://wallet:wallet@postgres:5432/wallet?sslmode=disable" up
```

## 💻 Running the Application

```bash
go run ./cmd/app
```
The application will read the `.env` file and start the HTTP server on port `8080`. Press `Ctrl+C` to gracefully shut down.

## 🔐 Authentication (Keycloak)

The API is protected via OAuth 2.0/OIDC. The local Keycloak is provisioned with two test providers (`provider-a` and `provider-b`).

To obtain an access token, use the helper script:

```bash
chmod +x scripts/get-token.sh
./scripts/get-token.sh provider-a
```

Example of an authenticated request:
```bash
TOKEN=$(./scripts/get-token.sh provider-a)
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/health/live
```

## 🧪 Tests

The project includes unit tests and integration tests (which spin up temporary Docker containers).

**Unit Tests:**
```bash
go test ./...
```

**Integration Tests (requires Docker running):**
```bash
go test -tags=integration ./...
```

## 🏛️ Architecture and Decisions

The main technical decisions (such as money handling, concurrency locks, idempotency, and Outbox pattern) are thoroughly documented in the [ARCHITECTURE.md](./ARCHITECTURE.md) file.