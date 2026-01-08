# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Resgate is a realtime API gateway written in Go that implements the RES protocol. It acts as a bridge between clients (via HTTP/WebSocket) and microservices (via NATS messaging system), handling real-time synchronization of data across all connected clients.

## Common Commands

### Build
```bash
go build
```

### Run Tests
```bash
go test ./...
```

### Run Single Test
```bash
go test -run TestName ./test
```

### Run Tests with Verbose Output
```bash
go test -v ./...
```

### Linting and Checks
```bash
./scripts/check.sh
```
This runs `gofmt`, `go vet`, and `staticcheck`. Install staticcheck first:
```bash
./scripts/install-checks.sh
```

### Format Code
```bash
gofmt -s -w .
```

### Run Locally
Requires a running NATS server:
```bash
./resgate --nats nats://127.0.0.1:4222
```

## Architecture

### Core Components

- **main.go**: Entry point, handles CLI flags and configuration loading, creates and starts the Service
- **server/service.go**: Main Service struct orchestrating HTTP server, WebSocket handler, MQ client, and cache
- **server/rescache/**: In-memory resource cache that tracks subscriptions and handles events from NATS
- **server/wsConn.go**: WebSocket connection handler managing client subscriptions
- **server/apiHandler.go**: HTTP REST API handler
- **nats/nats.go**: NATS messaging client implementation

### Request Flow

1. Clients connect via WebSocket (`/`) or HTTP (`/api/`)
2. Requests are forwarded to microservices over NATS with subjects like `call.{resource}.{method}`, `access.{resource}`, `get.{resource}`
3. Responses are cached and forwarded to clients
4. Events from microservices (`event.{resource}.*`) update the cache and notify subscribed clients

### Key Packages

- **server/codec/**: Message encoding/decoding for RES protocol
- **server/mq/**: Message queue interface (implemented by nats/)
- **server/reserr/**: RES protocol error types
- **server/metrics/**: OpenMetrics integration
- **logger/**: Logging interface and implementations

### Test Structure

Tests are primarily in the `test/` directory using a custom test harness:
- `test/test.go`: Core test session setup with mock NATS client
- `test/natstest.go`: Mock NATS implementation for testing
- Tests use table-driven patterns with `runTest()` helper function

### Protocol Version

Current: RES protocol v1.2.3 (see `server/const.go`)
