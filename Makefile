.PHONY: all setup data up up-llm smoke test reference-check llm-eval benchmark replay clean

SHELL := /bin/bash

all: setup data test smoke

setup:
	@echo "=== Building Rust simulation core (flysim) ==="
	cargo build --release --manifest-path rust/flysim/Cargo.toml
	@mkdir -p bin
	cp rust/flysim/target/release/flysim bin/flysim
	@echo "=== Building Go orchestrator & API server (flylab) ==="
	go build -o bin/flylab cmd/flylab/main.go
	@echo "=== Setup complete. Binaries installed in bin/ ==="

data:
	@./scripts/setup_data.sh

up:
	@if [ "$$(which bin/flylab 2>/dev/null || true)" ] || [ -f bin/flylab ]; then \
		echo "Starting FlyLab on http://127.0.0.1:8080 (Domain: flylab.aglabx.com)..."; \
		./bin/flylab; \
	else \
		echo "bin/flylab missing. Running make setup first..."; \
		$(MAKE) setup; \
		./bin/flylab; \
	fi

up-llm:
	@echo "Checking Ollama status..."
	@if curl -s http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then \
		echo "Ollama is running. Pulling qwen3:8b model..."; \
		curl -s -X POST http://127.0.0.1:11434/api/pull -d '{"name": "qwen3:8b"}'; \
	else \
		echo "Starting Ollama via docker compose profile 'llm'..."; \
		docker compose --profile llm up -d ollama; \
		docker compose exec ollama ollama pull qwen3:8b; \
	fi

smoke:
	@./scripts/smoke.sh

test:
	@echo "=== Running Go unit and integration tests ==="
	go test -v ./...
	@echo "=== Running Rust unit and micro tests ==="
	cargo test --manifest-path rust/flysim/Cargo.toml

reference-check:
	@./scripts/reference_check.sh

llm-eval:
	@./scripts/llm_eval.sh

benchmark:
	@./scripts/benchmark.sh

replay:
	@./scripts/replay.sh $${PLAN:-artifacts/demo_run}

clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	cargo clean --manifest-path rust/flysim/Cargo.toml
	rm -f flylab.db*
