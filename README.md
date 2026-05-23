# Distributed Payment System — v1

Learning project: build a payment processing system layer by layer (v1 → v8). v1 is a deliberately primitive monolith that crashes in 5 documented ways. See `docs/v1-spec.md` for the full spec.

## Prerequisites
- Docker + Docker Compose
- Go 1.23+
- `jq` (`brew install jq`)

## Quickstart
```bash
make up            # bring up postgres + payment-api
make verify        # confirm clean state (after ~10s for healthcheck)
make sim RPS=50 DURATION=30s EXPERIMENT_ID=00
make verify
make logs          # in another terminal
```

## Status

🚧 v1 in progress — files being implemented.

## Reset between experiments
```bash
make reset
```
