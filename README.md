# FlyLab: Drosophila Whole-Brain Connectome LIF Simulation Platform

[![Build & Test](https://img.shields.io/badge/tests-passing-brightgreen.svg)]()
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)]()
[![Rust](https://img.shields.io/badge/Rust-1.80+-orange?logo=rust&logoColor=white)]()
[![Dataset](https://img.shields.io/badge/FlyWire-v630_127K_neurons-blue)]()
[![Domain](https://img.shields.io/badge/domain-flylab.aglabx.com-purple)]()

FlyLab is a high-performance, deterministic computational neuroscience platform for simulating spiking neural network dynamics across the entire *Drosophila melanogaster* connectome (127,400 neurons and 14,687,178 synaptic connections).

Designed for deployment at **`flylab.aglabx.com`** and public distribution via [`ad3002/flylab`](https://github.com/ad3002/flylab), FlyLab ports the landmark *Nature* 2024 model ([Shiu et al. 2024](https://doi.org/10.1038/s41586-024-07982-0)) into a self-contained, memory-efficient **Rust** simulation core and a production-grade **Go** orchestration engine and web interface.

---

## 1. Key Architectural Features

- **Blazing Fast Simulation Engine (`rust/flysim`)**:
  - Compressed Sparse Row (CSR) graph format builds in 3.6 s and loads from binary cache in **< 0.1 s** (176 MB).
  - Simulates 100 ms of whole-brain dynamics (127.4K neurons, 14.68M synapses) across dual comparison conditions in **~1.0 second**.
  - Peak RSS memory footprint is only **~650 MB** (well within standard workstation limits, replacing the multi-gigabyte Python overhead).
  - Exact analytical linear integration of membrane potential and synaptic conductance, bit-for-bit identical to Brian2 reference runs.
- **Robust Orchestrator & HTTP API (`internal/api`)**:
  - Written in pure Go with zero npm, node, or CGO dependencies.
  - SQLite backend in WAL mode with atomic queue processing, crash recovery, and idempotent submission (`Idempotency-Key`).
  - Strict JSON Schema validation (Draft 2020-12) rejecting unknown fields or invalid parameter ranges.
- **Reproducible Scientific Archives**:
  - Every job packages an immutable ZIP export with `plan.json`, `resolved_plan.json`, `manifest.json`, `input_events.parquet`, `spikes.parquet`, `rates.csv`, `summary.json`, `report.md`, `replay.sh`, and `checksums.sha256`.
  - Replay can be executed on any offline machine with the standalone `flysim` CLI without Python, web servers, or network access.
- **Natural-Language Planner (`internal/llm`)**:
  - Requests in English or Russian are turned into a structured `ExperimentPlan` by the Claude Code CLI in print mode (`claude -p`, model `claude-sonnet-5-5` by default), constrained by a JSON schema generated from the neuron registry and the plan schema limits; the result is re-validated by the same validator as hand-written plans.
  - Claude decides `ready` / `needs_input` / `unsupported` (whole-animal behaviour such as walking or flight is out of scope); there are no keyword pre-filters.
  - No silent fallback: planner failures return `502 LLM_ERROR` with the reason; only a missing `claude` binary switches to a keyword parser, and that response carries a visible `llm_error`.
  - Global concurrency cap (`LLM_MAX_CONCURRENCY`, `503 LLM_BUSY`) and a per-user hourly limit (`PARSE_RATE_LIMIT_PER_HOUR`, `429 RATE_LIMITED`).
- **AI Hypotheses about a Run (`internal/interpret`, contract `docs/v3_interpretation.md`)**:
  - A deterministic digest of every succeeded run (totals, activity by FlyWire super class / cell class / neurotransmitter, top neurons with annotations, first-spike latency, synaptic hops and signed direct input from the stimulated and silenced sets via `flysim digest`, readouts with literature-backed behavioural proxies, annotation coverage, model facts) is sent to `claude -p` (`claude-opus-5-5` by default).
  - Claude returns observations and falsifiable hypotheses with calibrated confidence, evidence and a runnable follow-up plan; each plan is re-validated (`plan_id` or a visible `plan_error`), and evidence naming neuron ids absent from the digest is flagged in `evidence_warnings`. Every answer carries a fixed disclaimer: these are AI-generated hypotheses about a model, not biological findings.
  - Runs finished before v3 are interpretable without re-running: `digest_graph.json` is computed lazily from the stored `spikes.parquet` (a missing file is a visible `digest_error`).
- **Accounts & History**:
  - Username/password accounts (PBKDF2-SHA256, 210 000 iterations), 30-day sessions via an `HttpOnly` cookie or `Authorization: Bearer`.
  - Every job belongs to its creator; other users get `404`. `GET /api/v1/jobs` is a per-user history with the stored plan and a compact result summary (a corrupt `summary.json` is reported in `summary_error`, never dropped).
- **Zero-Build Web Interface (`web/`)**:
  - Responsive vanilla CSS/JS interface served directly by the Go binary.
  - Visualizes parameter review, real-time stage progress, tabular rate comparison ($\Delta$ Hz), and interactive spike rasters.

---

## 2. Scientific Model & Upstream Audit

FlyLab faithfully implements the Leaky Integrate-and-Fire (LIF) model published by Shiu, Hermundstad, Sterne, et al. in *Nature* 634, 1021–1032 (2024):
- **Upstream Source**: [`philshiu/Drosophila_brain_model`](https://github.com/philshiu/Drosophila_brain_model)
- **Pinned Commit**: `91bdd1e7dcf193f3e7ca5a8933497fcef63b7960`
- **Data Release**: FlyWire v630 Final (March 23, 2023)
  - Completeness CSV: `2023_03_23_completeness_630_final.csv` (127,400 neurons; SHA256: `e6b71e17671a9bdb05f55e4bc6774640a1418cb7a05125e0fc994ad40f9bfdfb`)
  - Connectivity Parquet: `2023_03_23_connectivity_630_final.parquet` (14,687,178 synapses; Brotli-compressed; SHA256: `94db8c650533bc36ffa3223f2e62325d5648b8d6bd31c3a4e1c804628c7557b3`)

### Mathematical Specification
For timestep $dt = 0.1\text{ ms}$:
$$\frac{dv}{dt} = \frac{(E_L - v) + g}{\tau_m}, \quad \frac{dg}{dt} = -\frac{g}{\tau_s}$$
- Resting potential ($E_L$) and Reset ($V_{\text{reset}}$): $-52\text{ mV}$
- Firing threshold ($V_{\text{th}}$): $-45\text{ mV}$
- Membrane time constant ($\tau_m$): $20\text{ ms}$
- Synaptic time constant ($\tau_s$): $5\text{ ms}$
- Absolute refractory period ($\tau_{\text{ref}}$): $2.2\text{ ms}$ (22 simulation ticks)
- Conduction delay: $1.8\text{ ms}$ (18 simulation ticks)
- Synaptic weight scale: $w_{\text{scale}} = 0.275\text{ mV}$

### Upstream Audit Finding: Silencing Semantics
The upstream repository README states that silencing sets all connections *to and from* target neurons to zero. However, audit of the actual upstream execution code (`model.py`):
```python
syn.w[' {} == i'.format(i)] = 0*mV
```
reveals that only **outgoing** connections are zeroed (`i` is the presynaptic index in Brian2). Presynaptic inputs still reach the silenced neurons. FlyLab implements the true execution semantics of the verified code and explicitly documents this behavior (see [`docs/upstream_audit.md`](docs/upstream_audit.md)).

---

## 3. Directory Structure

```
├── cmd/
│   └── flylab/              # Go main application entrypoint
├── internal/
│   ├── api/                 # HTTP API & Web UI routing
│   ├── config/              # Environment & configuration loader
│   ├── contracts/           # JSON schema & neuron registry validator
│   ├── domain/              # Core domain models
│   ├── export/              # ZIP archive generator
│   ├── auth/                # Password hashing, session tokens, account rules
│   ├── cli/                 # `flylab user create|passwd` subcommands
│   ├── llm/                 # Claude CLI planner (claude -p) & keyword fallback
│   ├── storage/             # SQLite WAL store & job queue
│   └── worker/              # Process runner for Rust flysim
├── rust/
│   └── flysim/              # Standalone Rust simulation engine
├── contracts/
│   ├── experiment-plan.schema.json
│   └── fixtures/            # Valid and invalid test schemas
├── registry/
│   ├── groups.json          # Predefined neuron groups (sugar_grn, mn9, etc.)
│   └── presets.json         # Standardized experimental presets
├── web/
│   ├── static/              # CSS, client-side JS (zero build, zero npm)
│   └── templates/           # Server-rendered HTML templates
├── scripts/
│   ├── setup_data.sh        # Dataset downloader & CSR graph cache builder
│   ├── smoke.sh             # Full end-to-end integration smoke test (registers a throwaway user)
│   ├── seed_demo.py         # Demo account + 20 realistic experiments over HTTP
│   ├── reference_check.sh   # Parity check against Brian2 golden traces
│   ├── llm_eval.sh          # Prompt parsing evaluation suite (real claude -p)
│   ├── benchmark.sh         # Performance & RSS memory benchmark
│   └── replay.sh            # Standalone offline replay tool
├── docs/
│   ├── upstream_audit.md    # Source code analysis & semantics audit
│   ├── model_semantics.md   # Mathematical stage mapping
│   ├── reference_run.md     # Reference Brian2 execution logs
│   ├── user_study_protocol.md # User study experimental design
│   └── llm_eval.md          # Natural language parsing evaluation
├── compose.yaml             # Docker Compose for production & services
├── Dockerfile               # Production multi-stage container
├── Dockerfile.reference     # Isolated Brian2 environment container
├── Makefile                 # Canonical operational commands
└── README.md
```

---

## 4. Quickstart Guide

### Prerequisites
- **Rust Toolchain**: `rustc` and `cargo` $\ge 1.80$
- **Go Toolchain**: `go` $\ge 1.26$ (stdlib `crypto/pbkdf2`)
- **Claude Code CLI** (`claude`) on `PATH` for the natural-language planner (optional: without it `/plans/parse` uses a keyword parser and says so in `llm_error`)
- **Disk Space**: ~450 MB for FlyWire data files, ~176 MB for CSR binary cache, ~30 MB for compiled binaries.

### 1. Build Binaries
```bash
make setup
```
Compiles `bin/flysim` (Rust release) and `bin/flylab` (Go server).

### 2. Verify Data & Build Connectome Cache
```bash
make data
```
Verifies SHA256 integrity of FlyWire data and builds `data/cache/flywire_630_csr.bin`. It also downloads the
FlyWire neuron annotations (Schlegel et al., Nature 2024; pinned commit and SHA-256 in
`data/dataset_manifest.json` → `files.annotations`) and derives `data/annotations_630.tsv` for the v630 root
ids (106,214 of 127,400 neurons, 83.4 %; ids edited between materializations 630 and 783 stay unannotated).
Without that file the server still runs, `/capabilities` reports `annotations_ready: false`, and every
digest carries a coverage warning.

### 3. Run Automated Tests & Smoke Verification
```bash
# Run unit & integration tests
make test

# Run full end-to-end smoke test
make smoke

# Run benchmark suite
make benchmark
```

### 4. Start Server
```bash
make up
```
Open your browser at **`http://127.0.0.1:8080`** (landing page) and **`/app`** (application).

### 5. Accounts and Demo Data
```bash
# create an account from the shell (works with REGISTRATION_OPEN=false too)
bin/flylab user create --username alice --password 'a-long-password' --display-name 'Alice'
bin/flylab user passwd --username alice --password 'a-new-password'   # also revokes her sessions

# fill a running server with a demo account and 20 finished experiments
python3 scripts/seed_demo.py --base-url http://127.0.0.1:8080 --password 'demo-password'
```

### Configuration

All settings are environment variables (see `.env.example`). A variable that is set but malformed
(e.g. `PORT=abc`) stops the server at startup instead of silently falling back to the default.

| Variable | Default | Meaning |
| --- | --- | --- |
| `HOST`, `PORT` | `0.0.0.0`, `8080` | listen address |
| `DB_PATH`, `DATA_DIR`, `ARTIFACTS_DIR` | `flylab.db`, `data`, `artifacts` | storage locations |
| `FLYSIM_BIN`, `MAX_WALL_SECONDS` | `bin/flysim`, `3600` | simulator binary and per-job wall limit |
| `CLAUDE_BIN`, `CLAUDE_MODEL` | `claude`, `claude-sonnet-5-5` | planner CLI and model |
| `CLAUDE_TIMEOUT_SECONDS` | `90` | hard timeout per parse |
| `LLM_MAX_CONCURRENCY` | `2` | concurrent `claude -p` processes (wait up to 30 s, then `503 LLM_BUSY`) |
| `PARSE_RATE_LIMIT_PER_HOUR` | `60` | per-user parse limit (`429 RATE_LIMITED`) |
| `REGISTRATION_OPEN` | `true` | when `false`, self-registration returns `403 REGISTRATION_CLOSED` |
| `PARSE_RATE_LIMIT_PER_IP_PER_HOUR` | `120` | parses per client address across all accounts (`429`, `details.scope=ip`) |
| `PARSE_GLOBAL_LIMIT_PER_HOUR` | `300` | parses for the whole server, i.e. paid Claude calls (`429`, `details.scope=global`) |
| `AUTH_RATE_LIMIT_PER_IP` | `30` | login + register attempts per client address per 15 min (`429`, `scope=ip`) |
| `LOGIN_FAILURES_PER_USERNAME` | `10` | failed logins per username per 15 min before it is locked for the window (`scope=username`) |
| `REGISTER_RATE_LIMIT_PER_IP_PER_HOUR` | `5` | accounts created per client address per hour (`scope=register_ip`) |
| `PASSWORD_HASH_CONCURRENCY` | `4` | concurrent PBKDF2 checks; more waiting than 2 s get `503 AUTH_BUSY` |
| `CLAUDE_INTERPRET_MODEL` | `claude-opus-5-5` | model of `POST /api/v1/jobs/{id}/interpretation` |
| `CLAUDE_INTERPRET_TIMEOUT_SECONDS` | `180` | hard timeout per interpretation call |
| `INTERPRET_RATE_LIMIT_PER_HOUR` | `20` | new interpretations per user per hour (cached answers are free; `429`, `scope=user`) |
| `INTERPRET_RATE_LIMIT_PER_IP_PER_HOUR` | `40` | new interpretations per client address per hour (`scope=ip`) |
| `INTERPRET_GLOBAL_LIMIT_PER_HOUR` | `100` | new interpretations for the whole server per hour (`scope=global`) |

---

## 5. HTTP API Walkthrough (cURL)

### 1. Health & Capabilities
```bash
# Health check
curl -s http://127.0.0.1:8080/health

# System capabilities & budget limits
curl -s http://127.0.0.1:8080/capabilities
```

### 2. Create an Account (session token)
```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:8080/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"a-long-password"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/me
```
Plan validation, `/health`, `/capabilities`, `/datasets`, `/groups` and `/neurons` are public;
parsing, job submission and every `/api/v1/jobs/...` endpoint need the token (or the
`flylab_session` cookie set by register/login).

### 3. Natural-Language Planning (Claude)
```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/plans/parse -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Compare sugar GRNs at 100 Hz with and without demo_silencing for 200 ms","report_language":"en"}'
```
Returns `status` (`ready` / `needs_input` / `unsupported`), the validated `plan` and
`resolved_plan` (with `plan_id`), and `llm_metadata = {source, model, duration_ms, cost_usd}`.

### 4. Query Available Neuron Groups
```bash
curl -s http://127.0.0.1:8080/groups
```

### 5. Validate Experiment Plan
```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/plans/validate \
  -H "Content-Type: application/json" \
  -d '{
    "schema_version": "1.0",
    "dataset_id": "flywire_630",
    "model_id": "shiu_lif_rust",
    "experiment_type": "compare_silencing",
    "activation": [
      {
        "selector": {"group_id": "sugar_grn"},
        "rate_hz": 50.0
      }
    ],
    "silencing": [
      {
        "selector": {"group_id": "demo_silencing"}
      }
    ],
    "readout": [
      {
        "selector": {"group_id": "mn9"}
      }
    ],
    "duration_ms": 100.0,
    "repeats": 1,
    "base_seed": 42,
    "report_language": "en"
  }'
```
Response:
```json
{
  "plan_id": "plan_9b38ead6",
  "plan_hash": "52aabec7090f21ef613a03d79cd049d02940387bcdf08072f8295fa4599f9d6a",
  "budget": {
    "estimated_wall_seconds": 2.5,
    "max_wall_seconds": 3600
  }
}
```

### 6. Submit Job to Worker Queue
```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/jobs -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: experiment-run-001" \
  -d '{"plan_id": "plan_9b38ead6", "title": "Sugar vs silencing", "prompt": "optional original request"}'
```
Idempotency keys are scoped per user.

### 7. Check Progress and History
```bash
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/jobs/{job_id}
# your jobs, newest first, with plan + summary (+ summary_error when summary.json is corrupt)
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/api/v1/jobs?limit=24&offset=0&status=succeeded"
```

### 8. Retrieve Results & Download Export ZIP
```bash
# Get summary rates and delta Hz
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/jobs/{job_id}/results

# Get spike data as JSON or CSV
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/api/v1/jobs/{job_id}/spikes?limit=50&format=json"

# Download complete self-contained reproducibility archive
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/jobs/{job_id}/export -o experiment_export.zip
```

### 9. AI Hypotheses about a Finished Run (v3)
```bash
# deterministic digest only (no LLM call); computes digest_graph.json for older runs on first use
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/jobs/{job_id}/digest

# generate (or return the cached) interpretation; regenerate=true replaces it
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"language":"ru"}' http://127.0.0.1:8080/api/v1/jobs/{job_id}/interpretation

# latest stored interpretation (404 INTERPRETATION_NOT_FOUND when there is none)
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/jobs/{job_id}/interpretation
```
A real interpretation takes about two minutes with `claude-opus-5-5`; keep reverse-proxy read timeouts
above `CLAUDE_INTERPRET_TIMEOUT_SECONDS` + 120 s.

---

## 6. Standalone Replay (Zero Python, Zero Web)

Any experiment archive downloaded from FlyLab can be completely reproduced on an air-gapped machine using only the compiled `flysim` binary:

```bash
# Replay via Makefile
make replay PLAN=artifacts/demo_run

# Or direct CLI call
bin/flysim replay \
  --resolved-plan artifacts/demo_run/resolved_plan.json \
  --input-events artifacts/demo_run/input_events.parquet \
  --manifest artifacts/demo_run/manifest.json \
  --output replayed_results \
  --cache-dir data/cache
```
Output:
```json
{
  "status": "replayed",
  "total_spikes_A": 267,
  "total_spikes_B": 266,
  "wall_seconds": 1.04
}
```

---

## 7. Performance Benchmarks

Run `make benchmark` to replicate these measurements on your hardware:

| Benchmark Metric | Upstream (Python/Brian2) | FlyLab (Rust Core) | Speedup / Reduction |
| :--- | :--- | :--- | :--- |
| **Connectome Initialization** | ~45 s | **< 0.1 s** (cached) | **450x faster** |
| **100 ms Simulation (A & B)** | ~22 s | **1.04 s** | **21x faster** |
| **Peak Memory (RSS)** | ~5.8 GB | **~650 MB** | **89% less memory** |
| **Spike Parity** | Baseline | **100% Identical** | Exact match |

---

## 8. License & Attribution

- **Connectome Dataset**: FlyWire consortium under CC-BY 4.0.
- **Upstream Code**: Pinned commit `91bdd1e` from Phil Shiu (preserved in `third_party/upstream/`).
- **FlyLab Codebase**: MIT License. See [LICENSE](LICENSE) for details.
