# Crypto Core

Multi-chain crypto payment core in Go: blockchain deposit detection, HD wallet derivation, confirmation tracking, gas/prefund management, TRON energy rental, and automated sweeping for Bitcoin, EVM chains, TRON, Solana, and TON.

## What is included

- HD wallet address derivation and address validation
- Bitcoin, EVM, TRON, Solana, and TON deposit scanners
- Deposit confirmation and transaction state management
- Gas wallet monitoring, prefunding, and residue sweeping
- Automated sweeps with retry/recovery handling
- TRON energy providers with fallback ordering
- Secure, signed webhook delivery and retry processing
- PostgreSQL persistence, Redis coordination, and focused migrations

## Deliberately excluded

This folder is the crypto-processing worker, not the application UI. It does not include the frontend, HTTP/API handlers, login/session/2FA/passkey code, admin or merchant dashboards, Telegram bots, notifications UI, payouts, or withdrawals.

The worker expects another service to create payment, wallet, and deposit records in PostgreSQL. It then detects chain activity, updates those records, sends webhooks, and performs sweeps.

## Runtime requirements

- Go 1.25+
- PostgreSQL
- Redis
- RPC/API access for the networks you enable
- A stable `APP_SECRET` matching the key used to encrypt wallet and gas-wallet secrets

## Run locally

```sh
cp .env.example .env
# Edit .env with real database, Redis, RPC, and provider values.
go mod download
go test ./...
go run ./cmd/worker
```

The worker applies the migrations in `migrations/` on startup. Run it from this directory. Use a dedicated database for this standalone project, or reconcile the migration history before connecting it to an existing gateway database.

## Configuration

Start with `.env.example`. RPC variables are optional per network; unset networks are skipped. TRON providers accept comma-separated entries in this format:

```text
name|target|api-key
```

For example: `primary|grpc.example.com:50051|token`. Never commit `.env`, private keys, encrypted seed material, or provider credentials.

## Checks

```sh
make test
make vet
```

## GitHub description

Multi-chain crypto payment core in Go: deposit detection, HD wallet derivation, confirmations, gas/prefund management, TRON energy rental, and automated sweeping for Bitcoin, EVM, TRON, Solana, and TON.
