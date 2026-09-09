.PHONY: up down build logs migrate seed smoke bench test test-gateway test-controlplane test-advisor test-reconciliation clean

COMPOSE := docker compose -f deploy/docker-compose.yml

up:
	$(COMPOSE) up -d --build
	@echo "Waiting for services to become healthy..."
	@sleep 3
	@echo "Gateway:      http://localhost:8080"
	@echo "Control plane: http://localhost:8081"
	@echo "Advisor:      http://localhost:8082"
	@echo "Prometheus:   http://localhost:9091"
	@echo "Grafana:      http://localhost:3000 (admin/admin)"

down:
	$(COMPOSE) down

build:
	$(COMPOSE) build

logs:
	$(COMPOSE) logs -f

migrate:
	$(COMPOSE) run --rm migrate

seed:
	python3 scripts/seed.py

smoke:
	bash scripts/smoke.sh

bench:
	bash scripts/benchmark.sh

# Runs every service's own test suite. Each has its own Python venv
# (controlplane/.venv, advisor/.venv, reconciliation/.venv) -- create
# them first with `python3 -m venv <dir> && pip install -e ".[dev]"` if
# this is a fresh checkout; the Go module downloads its own deps via
# `go test` directly.
test: test-gateway test-controlplane test-advisor test-reconciliation

test-gateway:
	cd gateway && go build ./... && go test ./...

test-controlplane:
	cd controlplane && . .venv/bin/activate && PYTHONPATH=. pytest -q

test-advisor:
	cd advisor && . .venv/bin/activate && pytest -q

test-reconciliation:
	cd reconciliation && . .venv/bin/activate && pytest -q

clean: down
	rm -rf loadtest/results/*.json
