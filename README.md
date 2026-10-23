# Backend Challenge — Distributed Wagering Processing in Go

This project implements a financial processing service for wagering, built to withstand high concurrency, network failures, and ensure transactional consistency, persistent idempotency, and ledger integrity.

## 🛠️ Prerequisites

- **Go** (version 1.26 or higher)
- **Docker** and **Docker Compose** (Docker Desktop on Windows/Mac)
- **Git Bash** (recommended for Windows users, as the shell commands are bash-based)

## ⚙️ Environment Variables

Before starting the services, create the `.env` file from the example:

```bash
cp .env.example .env
```

## 🚀 Running the Infrastructure

The local environment spins up PostgreSQL, Keycloak, and LocalStack (SQS emulator) via Docker Compose.

```bash
docker compose up -d
```

Verify that the services are healthy:
```bash
docker compose ps
```

## 🗄️ Database Migrations

To create the database structure (tables, constraints, immutability triggers), run the migrations:

**Option 1: Using Make (Mac/Linux)**
```bash
make migrate-up
```

**Option 2: Using Docker directly (Windows / without Make)**
```bash
MSYS_NO_PATHCONV=1 docker run --rm --network backend-challenge-go_default -v "$(pwd)/migrations:/migrations" migrate/migrate:v4.18.1 -path=/migrations -database "postgres://wallet:wallet@postgres:5432/wallet?sslmode=disable" up
```

## 🔐 Authentication (Keycloak)

The API is protected via OAuth 2.0/OIDC. The local Keycloak is provisioned with two test providers (`provider-a` and `provider-b`) and an internal admin client.

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

## 💻 Running the Application

### Local Development (with Go)
```bash
go run ./cmd/app
```

### Using Docker Compose (Full Environment)
```bash
docker compose up --build
```

## 🧪 Tests

### Unit Tests
```bash
go test ./...
```

### Race Condition Tests (Concurrency)
```bash
go test -race ./...
```

### Integration Tests (Requires Docker running)
```bash
go test -tags=integration ./...
```
> **Windows note:** if `go test -tags=integration ./...` panics with `rootless Docker is not supported on Windows`, run the tests with the Docker named pipe explicitly and serialize packages:
> ```bash
> export DOCKER_HOST=npipe:////./pipe/docker_engine
> go test -tags=integration -count=1 -p 1 ./...
> ```
> On Linux and macOS, no override is needed.

### Vet and Format
```bash
go vet ./...
```

### End-to-End Test (Multi-Instance + Advanced Scenarios)
This test spins up 3 instances behind an Nginx load balancer and covers:
- HTTP + SQS sending the **same operation** (idempotency across transports).
- Poison message routing to DLQ.
- Consumer crash with **real SQS redelivery**.

```bash
# Ensure Docker is running
chmod +x tests/e2e/e2e.sh
./tests/e2e/e2e.sh
```

Expected output ends with:
```
=== E2E ADVANCED TEST PASSED SUCCESSFULLY ===
```

## 📦 Provisioning SQS Queues

After LocalStack is up, create the FIFO queues (inbound, outbound, and DLQ):

```bash
bash scripts/provision-queues.sh
```

## 🏛️ Architecture and Decisions

The main technical decisions (money handling, concurrency locks, idempotency, SQS/Inbox/Outbox, metrics, and Fx lifecycle) are thoroughly documented in the [ARCHITECTURE.md](./ARCHITECTURE.md) file.