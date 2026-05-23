.PHONY: help build up down logs sim verify reset psql clean

DB_URL ?= postgres://payment:payment_dev@localhost:5432/payment?sslmode=disable
RPS ?= 50
WORKERS ?= 10
DURATION ?= 30s
EXPERIMENT_ID ?= 00

help:  ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build:  ## Build all Go binaries to ./bin
	mkdir -p bin
	go build -o bin/payment-api ./cmd/payment-api
	go build -o bin/simulator   ./cmd/simulator
	go build -o bin/verifier    ./cmd/verifier

up:  ## Bring up stack (postgres + payment-api)
	docker compose up -d --build

down:  ## Stop stack (keep data)
	docker compose down

logs:  ## Tail payment-api logs through jq
	docker logs -f payment-api | jq .

sim: build  ## Run simulator (override RPS, WORKERS, DURATION, EXPERIMENT_ID)
	mkdir -p experiments/v1/data
	./bin/simulator \
	  --target=http://localhost:8080 \
	  --rps=$(RPS) --workers=$(WORKERS) --duration=$(DURATION) \
	  --output=experiments/v1/data/$(EXPERIMENT_ID)-sim-requests.jsonl

verify: build  ## Run verifier against current DB state
	./bin/verifier --db="$(DB_URL)"

reset:  ## Nuke DB volume and restart
	docker compose down -v
	docker compose up -d --build

psql:  ## Open psql shell inside postgres container
	docker exec -it payment-postgres psql -U payment -d payment

clean:  ## Remove build artifacts
	rm -rf bin/
