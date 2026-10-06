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
- **Local LLM Integration (`internal/llm`)**:
  - Native integration with local Ollama instances (`qwen3:8b`) to parse natural language requests into structured experiment plans.
  - Built-in heuristic NLP fallback parser ensuring 100% offline functionality if local LLMs are unavailable.
  - Zero biological hallucination: rejects whole-animal behavioral queries (`walking`, `flight`) that exceed spiking network scope.
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
│   ├── llm/                 # Ollama client & NLP fallback parser
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
│   ├── smoke.sh             # Full end-to-end integration smoke test
│   ├── reference_check.sh   # Parity check against Brian2 golden traces
│   ├── llm_eval.sh          # Prompt parsing evaluation suite
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
- **Go Toolchain**: `go` $\ge 1.22$
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
Verifies SHA256 integrity of FlyWire data and builds `data/cache/flywire_630_csr.bin`.

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
Open your browser at **`http://127.0.0.1:8080`** (or domain `flylab.aglabx.com`).

---

## 5. HTTP API Walkthrough (cURL)

### 1. Health & Capabilities
```bash
# Health check
curl -s http://127.0.0.1:8080/health

# System capabilities & budget limits
curl -s http://127.0.0.1:8080/capabilities
```

### 2. Query Available Neuron Groups
```bash
curl -s http://127.0.0.1:8080/groups
```

### 3. Validate Experiment Plan
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

### 4. Submit Job to Worker Queue
```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: experiment-run-001" \
  -d '{"plan_id": "plan_9b38ead6"}'
```

### 5. Check Progress
```bash
curl -s http://127.0.0.1:8080/api/v1/jobs/{job_id}
```

### 6. Retrieve Results & Download Export ZIP
```bash
# Get summary rates and delta Hz
curl -s http://127.0.0.1:8080/api/v1/jobs/{job_id}/results

# Get spike data as JSON or CSV
curl -s "http://127.0.0.1:8080/api/v1/jobs/{job_id}/spikes?limit=50&format=json"

# Download complete self-contained reproducibility archive
curl -s http://127.0.0.1:8080/api/v1/jobs/{job_id}/export -o experiment_export.zip
```

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
