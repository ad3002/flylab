# Reference Control Run & Verification Report

This document records the exact benchmark measurements from executing the published Drosophila brain spiking neural network model on FlyWire Materialization 630 using Brian2 in the isolated reference environment.

---

## 1. System & Environment Specifications

- **Reference Platform**: Brian 2 (version 2.5.1) on Python 3.10.13
- **Execution Target**: NumPy analytical code generation (`prefs.codegen.target = "numpy"`)
- **Host Architecture**: Apple Silicon (arm64, macOS 15.x Darwin)
- **Target Linux Benchmark Profile**: 8 CPU cores, 32 GB RAM, x86_64 / aarch64
- **Connectome Dataset**: FlyWire v630 (`2023_03_23_completeness_630_final.csv`, `2023_03_23_connectivity_630_final.parquet`)

---

## 2. Benchmark Parameters

| Parameter | Value | Description |
|---|---|---|
| `experiment_type` | `compare_silencing` | Condition A (stimulation only) vs Condition B (stimulation + silencing) |
| `duration_ms` | $100.0\text{ ms}$ | $1000$ integration timesteps ($dt = 0.1\text{ ms}$) |
| `activation_group` | `sugar_grn` ($21$ neurons) | Labellar sugar-sensing receptor neurons stimulated at $50.0\text{ Hz}$ |
| `silenced_group` | `demo_silencing` ($1$ neuron) | Root ID `720575940616885538` (outgoing synapses zeroed) |
| `readout_group` | `mn9` ($2$ neurons) | Bilateral motor neuron pair `720575940645521262` and `720575940660219265` |
| `repeats` | $1$ | Single trial |
| `base_seed` | $42$ | Seed for generating identical deterministic Bernoulli stimulus |

---

## 3. Measured Runtime & Resource Consumption

| Phase | Metric | Condition A | Condition B |
|---|---|---|---|
| Connectome Loading | Wall time | $0.94\text{ s}$ | (reused from cache) |
| Network Construction | Wall time | $0.65\text{ s}$ | $0.87\text{ s}$ |
| Simulation Execution | Wall time | $1.99\text{ s}$ | $2.56\text{ s}$ |
| Spike Output Count | Spikes emitted | $267\text{ spikes}$ | $260\text{ spikes}$ |
| Active Neurons Count | Neurons with $\ge 1$ spike | $82\text{ neurons}$ | $80\text{ neurons}$ |
| Peak Memory Usage | Process RSS | $\sim 2.2\text{ GB}$ | $\sim 2.3\text{ GB}$ |

---

## 4. Scientific Activity Results

### Stimulated Input Events
- Total generated Poisson input events across the 21 `sugar_grn` neurons: **126 events** ($t \in [0, 100]\text{ ms}$).

### Readout Motor Neurons (MN9)
At 50 Hz stimulation for 100 ms, the proboscis motor neurons remain below spiking threshold ($0.0\text{ Hz}$ in both conditions). This is recorded as a valid zero firing rate:
- `720575940645521262`: $0.0\text{ Hz}$ (Condition A) $\rightarrow 0.0\text{ Hz}$ (Condition B), $\Delta = 0.0\text{ Hz}$
- `720575940660219265`: $0.0\text{ Hz}$ (Condition A) $\rightarrow 0.0\text{ Hz}$ (Condition B), $\Delta = 0.0\text{ Hz}$

### Downstream Network Silencing Impact
Silencing the single sugar receptor neuron `720575940616885538` in Condition B directly reduces firing rates in downstream interneurons:
- Neuron `720575940629888530`: $80.0\text{ Hz} \rightarrow 70.0\text{ Hz}$ ($\Delta = -10.0\text{ Hz}$, $-12.5\%$)
- Neuron `720575940619973712`: $60.0\text{ Hz} \rightarrow 50.0\text{ Hz}$ ($\Delta = -10.0\text{ Hz}$, $-16.7\%$)
- Neuron `720575940620874757`: $50.0\text{ Hz} \rightarrow 40.0\text{ Hz}$ ($\Delta = -10.0\text{ Hz}$, $-20.0\%$)

Total network spike count decreased from 267 spikes to 260 spikes ($\Delta = -7$ spikes).

---

## 5. Artifact Reference Traces

The generated reference artifacts are stored in `reference/traces/`:
- `reference/traces/micro/micro_decay.csv`: Single neuron analytical decay trace
- `reference/traces/micro/micro_synapse.csv`: 2-neuron delayed synaptic transmission trace
- `reference/traces/control/input_events.parquet`: Seed 42 input event sequence
- `reference/traces/control/spikes.parquet`: Complete spike raster
- `reference/traces/control/rates.csv`: Firing rates per neuron
- `reference/traces/control/comparison.csv`: Paired differential analysis
- `reference/traces/control/summary.json`: High-level numeric summary
- `reference/traces/control/metrics.json`: Performance and resource timings
