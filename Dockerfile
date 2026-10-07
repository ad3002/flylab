# Multi-stage build for FlyLab (Rust Simulation Core + Go Orchestrator & Web UI)

# Stage 1: Build Rust flysim binary
FROM rust:1.82-bookworm AS rust-builder
WORKDIR /build/rust
COPY rust/flysim /build/rust/flysim
RUN cd flysim && cargo build --release

# Stage 2: Build Go flylab binary
FROM golang:1.26-bookworm AS go-builder
WORKDIR /build/go
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o flylab cmd/flylab/main.go

# Stage 3: Runtime container
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy binaries
COPY --from=rust-builder /build/rust/flysim/target/release/flysim /app/bin/flysim
COPY --from=go-builder /build/go/flylab /app/bin/flylab

# Copy static assets and configurations
COPY contracts /app/contracts
COPY registry /app/registry
COPY web /app/web
COPY data/dataset_manifest.json /app/data/dataset_manifest.json

# Environment defaults
ENV HOST=0.0.0.0 \
    PORT=8080 \
    DOMAIN=flylab.aglabx.com \
    DB_PATH=/app/data/flylab.db \
    DATA_DIR=/app/data \
    ARTIFACTS_DIR=/app/artifacts \
    REGISTRY_DIR=/app/registry \
    CONTRACTS_DIR=/app/contracts \
    WEB_DIR=/app/web \
    FLYSIM_BIN=/app/bin/flysim \
    CLAUDE_BIN=claude \
    CLAUDE_MODEL=claude-sonnet-5-5 \
    CLAUDE_TIMEOUT_SECONDS=90 \
    LLM_MAX_CONCURRENCY=2 \
    PARSE_RATE_LIMIT_PER_HOUR=60 \
    REGISTRATION_OPEN=true

# The natural-language planner shells out to the Claude Code CLI (`claude -p`). The image does
# not ship it: install it in a derived image or mount it and point CLAUDE_BIN at it. Without it
# /plans/parse uses the keyword parser and every response carries a visible `llm_error`.

EXPOSE 8080

ENTRYPOINT ["/app/bin/flylab"]
