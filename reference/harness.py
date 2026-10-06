"""Reference verification harness using Brian2 to generate golden traces for Rust flysim.

Usage:
    python harness.py micro --output-dir ./traces/micro
    python harness.py control --output-dir ./traces/control --duration 100 --seed 42
"""

import argparse
import json
import os
import sys
import time
from pathlib import Path

import numpy as np
import pandas as pd
import brian2
from brian2 import *

prefs.codegen.target = "numpy"

DEFAULT_PARAMS = {
    "v_0": -52.0 * mV,
    "v_rst": -52.0 * mV,
    "v_th": -45.0 * mV,
    "t_mbr": 20.0 * ms,
    "tau": 5.0 * ms,
    "t_rfc": 2.2 * ms,
    "t_dly": 1.8 * ms,
    "w_syn": 0.275 * mV,
    "f_poi": 250.0,
    "dt": 0.1 * ms,
}


def get_equations():
    return """
    dv/dt = (v_0 - v + g) / t_mbr : volt (unless refractory)
    dg/dt = -g / tau : volt (unless refractory)
    rfc : second
    """


def run_micro_checks(output_dir: Path):
    """Generates analytical single neuron, delayed synapse, and inhibitory micro traces."""
    output_dir.mkdir(parents=True, exist_ok=True)
    eqs = get_equations()

    # 1. Single neuron decaying to rest
    neu = NeuronGroup(
        1,
        eqs,
        method="linear",
        threshold="v > v_th",
        reset="v = v_rst; g = 0 * mV",
        refractory="rfc",
        namespace=DEFAULT_PARAMS,
    )
    neu.v = -48.0 * mV  # above rest, below threshold
    neu.g = 2.0 * mV
    neu.rfc = DEFAULT_PARAMS["t_rfc"]

    mon = StateMonitor(neu, ("v", "g", "not_refractory"), record=True)
    net = Network(neu, mon)
    net.run(5.0 * ms)

    df_decay = pd.DataFrame(
        {
            "step": np.arange(len(mon.t)),
            "t_ms": np.round(mon.t / ms, 2),
            "v_mv": np.array(mon.v[0] / mV),
            "g_mv": np.array(mon.g[0] / mV),
            "not_refractory": np.array(mon.not_refractory[0], dtype=bool),
        }
    )
    df_decay.to_csv(output_dir / "micro_decay.csv", index=False)

    # 2. Delayed 2-neuron synapse with spike and refractory
    neu2 = NeuronGroup(
        2,
        eqs,
        method="linear",
        threshold="v > v_th",
        reset="v = v_rst; g = 0 * mV",
        refractory="rfc",
        namespace=DEFAULT_PARAMS,
    )
    neu2.v = DEFAULT_PARAMS["v_0"]
    neu2.g = 0.0 * mV
    neu2.rfc = DEFAULT_PARAMS["t_rfc"]

    syn = Synapses(
        neu2,
        neu2,
        "w : volt",
        on_pre="g += w",
        delay=DEFAULT_PARAMS["t_dly"],
        name="test_syn",
    )
    syn.connect(i=[0], j=[1])
    syn.w = [5.0 * mV]

    # Force neuron 0 to spike at step 0
    neu2[0].v = 0.0 * mV

    spk_mon = SpikeMonitor(neu2)
    state_mon = StateMonitor(neu2, ("v", "g", "not_refractory"), record=True)
    net2 = Network(neu2, syn, spk_mon, state_mon)
    net2.run(5.0 * ms)

    df_syn = pd.DataFrame(
        {
            "step": np.arange(len(state_mon.t)),
            "t_ms": np.round(state_mon.t / ms, 2),
            "v0_mv": np.array(state_mon.v[0] / mV),
            "g0_mv": np.array(state_mon.g[0] / mV),
            "nr0": np.array(state_mon.not_refractory[0], dtype=bool),
            "v1_mv": np.array(state_mon.v[1] / mV),
            "g1_mv": np.array(state_mon.g[1] / mV),
            "nr1": np.array(state_mon.not_refractory[1], dtype=bool),
        }
    )
    df_syn.to_csv(output_dir / "micro_synapse.csv", index=False)

    print(f"Micro traces generated successfully in {output_dir}")


def generate_input_events(stimulated_ids, duration_ms, rate_hz, base_seed, dt_ms=0.1):
    """Generates Poisson/Bernoulli discrete input events matching PoissonInput(N=1, rate*dt)."""
    rng = np.random.default_rng(base_seed)
    total_ticks = int(round(duration_ms / dt_ms))
    p = rate_hz * (dt_ms / 1000.0)

    events = []
    for s_id in stimulated_ids:
        # Bernoulli trials for N=1
        draws = rng.binomial(1, p, size=total_ticks)
        for tick in np.where(draws > 0)[0]:
            events.append(
                {
                    "trial": 0,
                    "root_id": str(s_id),
                    "timestep": int(tick),
                    "multiplicity": int(draws[tick]),
                }
            )

    df_events = pd.DataFrame(events)
    if not df_events.empty:
        df_events.sort_values(by=["timestep", "root_id"], inplace=True)
    return df_events


def run_control_experiment(output_dir: Path, duration_ms: float = 100.0, rate_hz: float = 50.0, seed: int = 42):
    """Executes the reference control comparison experiment between Condition A and Condition B."""
    output_dir.mkdir(parents=True, exist_ok=True)

    # 1. Load registry and datasets
    with open("registry/groups.json", "r") as f:
        groups_data = json.load(f)
    groups = {g["group_id"]: g["neuron_ids"] for g in groups_data["groups"]}

    sugar_ids = groups["sugar_grn"]
    mn9_ids = groups["mn9"]
    silence_ids = groups["demo_silencing"]

    path_comp = Path("data/2023_03_23_completeness_630_final.csv")
    path_con = Path("data/2023_03_23_connectivity_630_final.parquet")

    if not path_comp.exists() or not path_con.exists():
        print(f"Error: dataset files missing from data/. Run 'make data' first.")
        sys.exit(1)

    t_start_total = time.time()
    print("Loading connectome tables...")
    t0 = time.time()
    df_comp = pd.read_csv(path_comp, index_col=0)
    flyid2i = {str(j): i for i, j in enumerate(df_comp.index)}
    i2flyid = {i: str(j) for i, j in enumerate(df_comp.index)}

    df_con = pd.read_parquet(path_con)
    print(f"Loaded in {time.time() - t0:.2f}s")

    # 2. Generate input events
    print("Generating deterministic input events...")
    df_events = generate_input_events(sugar_ids, duration_ms, rate_hz, seed)
    df_events.to_parquet(output_dir / "input_events.parquet", index=False)
    with open(output_dir / "input_events.json", "w") as f:
        json.dump(df_events.to_dict(orient="records"), f, indent=2)
    print(f"Generated {len(df_events)} input events.")

    # Convert events to map for fast lookup per tick
    events_by_tick = {}
    for _, row in df_events.iterrows():
        tick = int(row["timestep"])
        idx = flyid2i[str(row["root_id"])]
        mult = int(row["multiplicity"])
        events_by_tick.setdefault(tick, []).append((idx, mult))

    def run_sim(condition_name, silenced_neuron_ids):
        print(f"Building simulation for {condition_name}...")
        t_bld_start = time.time()
        neu = NeuronGroup(
            len(df_comp),
            DEFAULT_PARAMS["eqs"] if "eqs" in DEFAULT_PARAMS else get_equations(),
            method="linear",
            threshold="v > v_th",
            reset="v = v_rst; g = 0 * mV",
            refractory="rfc",
            namespace=DEFAULT_PARAMS,
        )
        neu.v = DEFAULT_PARAMS["v_0"]
        neu.g = 0.0 * mV
        neu.rfc = DEFAULT_PARAMS["t_rfc"]

        # Poisson targets have 0 refractory period
        for s_id in sugar_ids:
            neu[flyid2i[s_id]].rfc = 0.0 * ms

        syn = Synapses(
            neu,
            neu,
            "w : volt",
            on_pre="g += w",
            delay=DEFAULT_PARAMS["t_dly"],
        )
        i_pre = df_con["Presynaptic_Index"].values
        i_post = df_con["Postsynaptic_Index"].values
        syn.connect(i=i_pre, j=i_post)

        weights = df_con["Excitatory x Connectivity"].values.astype(float) * (DEFAULT_PARAMS["w_syn"] / mV)
        # Apply silencing to outgoing synapses: set weights where i_pre in silenced indices to 0.0
        if silenced_neuron_ids:
            silenced_indices = set(flyid2i[s_id] for s_id in silenced_neuron_ids)
            mask = np.isin(i_pre, list(silenced_indices))
            weights[mask] = 0.0

        syn.w = weights * mV

        # Event injector runner at slot 'synapses', order 0
        weight_step = DEFAULT_PARAMS["w_syn"] * DEFAULT_PARAMS["f_poi"]

        @network_operation(when="synapses")
        def inject_stimulus(t):
            tick = int(round(float(t / (0.1 * ms))))
            if tick in events_by_tick:
                for idx, mult in events_by_tick[tick]:
                    neu.v[idx] += mult * weight_step

        spk_mon = SpikeMonitor(neu)
        net = Network(neu, syn, inject_stimulus, spk_mon)
        t_bld = time.time() - t_bld_start

        print(f"Running {condition_name} for {duration_ms}ms...")
        t_sim_start = time.time()
        net.run(duration_ms * ms)
        t_sim = time.time() - t_sim_start
        print(f"{condition_name} finished in {t_sim:.2f}s, recorded {spk_mon.num_spikes} spikes.")

        spikes_dict = spk_mon.spike_trains()
        spike_records = []
        for brian_idx, times in spikes_dict.items():
            if len(times) > 0:
                root_id = i2flyid[brian_idx]
                for t in times:
                    spike_records.append(
                        {
                            "condition": condition_name,
                            "trial": 0,
                            "root_id": root_id,
                            "spike_time_ms": float(t / ms),
                        }
                    )

        return spike_records, t_bld, t_sim

    # Run Condition A (no silencing)
    spikes_A, bld_A, sim_A = run_sim("A", [])

    # Run Condition B (with silencing)
    spikes_B, bld_B, sim_B = run_sim("B", silence_ids)

    # 3. Aggregate spikes and calculate rates
    all_spikes = spikes_A + spikes_B
    df_spikes = pd.DataFrame(all_spikes)
    if not df_spikes.empty:
        df_spikes.sort_values(by=["condition", "spike_time_ms", "root_id"], inplace=True)
    df_spikes.to_parquet(output_dir / "spikes.parquet", index=False)

    # Compute rates for readout neurons (MN9) and all active neurons
    duration_sec = duration_ms / 1000.0
    rate_records = []

    # Include all readout neurons, guaranteeing 0.0 Hz if no spikes
    for cond, spk_list in [("A", spikes_A), ("B", spikes_B)]:
        counts = {}
        for s in spk_list:
            r_id = s["root_id"]
            counts[r_id] = counts.get(r_id, 0) + 1

        for r_id in mn9_ids:
            c = counts.get(r_id, 0)
            rate_records.append(
                {
                    "condition": cond,
                    "trial": 0,
                    "root_id": r_id,
                    "spike_count": c,
                    "rate_hz": float(c / duration_sec),
                    "is_readout": True,
                }
            )

        for r_id, c in counts.items():
            if r_id not in mn9_ids:
                rate_records.append(
                    {
                        "condition": cond,
                        "trial": 0,
                        "root_id": r_id,
                        "spike_count": c,
                        "rate_hz": float(c / duration_sec),
                        "is_readout": False,
                    }
                )

    df_rates = pd.DataFrame(rate_records)
    df_rates.to_csv(output_dir / "rates.csv", index=False)

    # 4. Compute Comparison
    comp_records = []
    rates_A = {r["root_id"]: r["rate_hz"] for r in rate_records if r["condition"] == "A"}
    rates_B = {r["root_id"]: r["rate_hz"] for r in rate_records if r["condition"] == "B"}
    all_ids = set(rates_A.keys()).union(rates_B.keys())

    for r_id in all_ids:
        r_a = rates_A.get(r_id, 0.0)
        r_b = rates_B.get(r_id, 0.0)
        delta = r_b - r_a
        rel_change = (delta / r_a) if r_a > 0.0 else None
        comp_records.append(
            {
                "root_id": r_id,
                "is_readout": r_id in mn9_ids,
                "rate_A_hz": r_a,
                "rate_B_hz": r_b,
                "delta_hz": delta,
                "relative_change": rel_change,
            }
        )

    df_comp = pd.DataFrame(comp_records)
    df_comp.sort_values(by=["is_readout", "rate_A_hz"], ascending=[False, False], inplace=True)
    df_comp.to_csv(output_dir / "comparison.csv", index=False)

    # 5. Metrics & Summary
    metrics = {
        "engine": "brian2",
        "python_version": sys.version,
        "brian2_version": brian2.__version__,
        "codegen_target": prefs.codegen.target,
        "duration_ms": duration_ms,
        "rate_hz": rate_hz,
        "base_seed": seed,
        "build_time_A_s": bld_A,
        "sim_time_A_s": sim_A,
        "build_time_B_s": bld_B,
        "sim_time_B_s": sim_B,
        "total_wall_s": time.time() - t_start_total,
        "total_spikes_A": len(spikes_A),
        "total_spikes_B": len(spikes_B),
    }
    with open(output_dir / "metrics.json", "w") as f:
        json.dump(metrics, f, indent=2)

    summary = {
        "dataset_id": "flywire_630",
        "experiment_type": "compare_silencing",
        "duration_ms": duration_ms,
        "readout_neurons": mn9_ids,
        "readout_summary": [
            {
                "root_id": r_id,
                "rate_A_hz": rates_A.get(r_id, 0.0),
                "rate_B_hz": rates_B.get(r_id, 0.0),
                "delta_hz": rates_B.get(r_id, 0.0) - rates_A.get(r_id, 0.0),
            }
            for r_id in mn9_ids
        ],
        "active_neurons_count_A": len([k for k, v in rates_A.items() if v > 0]),
        "active_neurons_count_B": len([k for k, v in rates_B.items() if v > 0]),
    }
    with open(output_dir / "summary.json", "w") as f:
        json.dump(summary, f, indent=2)

    print(f"Reference control run completed successfully in {output_dir}")


def main():
    parser = argparse.ArgumentParser(description="FlyLab Reference Verification Harness")
    subparsers = parser.add_subparsers(dest="cmd", required=True)

    micro_parser = subparsers.add_parser("micro", help="Run analytical micro-checks")
    micro_parser.add_argument("--output-dir", type=Path, default=Path("reference/traces/micro"))

    ctrl_parser = subparsers.add_parser("control", help="Run full connectome control experiment")
    ctrl_parser.add_argument("--output-dir", type=Path, default=Path("reference/traces/control"))
    ctrl_parser.add_argument("--duration", type=float, default=100.0)
    ctrl_parser.add_argument("--rate", type=float, default=50.0)
    ctrl_parser.add_argument("--seed", type=int, default=42)

    args = parser.parse_args()
    if args.cmd == "micro":
        run_micro_checks(args.output_dir)
    elif args.cmd == "control":
        run_control_experiment(args.output_dir, args.duration, args.rate, args.seed)


if __name__ == "__main__":
    main()
