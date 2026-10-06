use std::collections::{HashMap, HashSet};
use std::fs::File;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::Instant;

use clap::{Parser, Subcommand};
use serde_json::json;
use sha2::{Digest, Sha256};

use flysim::graph::ConnectomeGraph;
use flysim::input::{
    generate_deterministic_events, index_events_by_tick, load_events_from_parquet,
    save_events_to_parquet,
};
use flysim::manifest::{DatasetManifest, ResolvedPlan};
use flysim::model::{AnalyticalCoefficients, LifParameters, NetworkState};
use flysim::output::{
    generate_checksums, save_comparison_csv, save_rates_csv, save_spikes_parquet,
    ComparisonRow, NeuronRate, OutputSpike,
};

#[derive(Parser)]
#[command(name = "flysim")]
#[command(about = "Drosophila Connectome LIF Spiking Neural Network Simulator", long_about = None)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Inspect dataset files and verify manifest checksums
    Inspect {
        #[arg(long)]
        dataset_manifest: PathBuf,
    },
    /// Prepare and cache binary connectome graph
    Prepare {
        #[arg(long)]
        dataset_manifest: PathBuf,
        #[arg(long)]
        output: PathBuf,
    },
    /// Run simulation according to a resolved experiment plan
    Run {
        #[arg(long)]
        resolved_plan: PathBuf,
        #[arg(long)]
        output: PathBuf,
        #[arg(long)]
        cache_dir: Option<PathBuf>,
        #[arg(long)]
        manifest: Option<PathBuf>,
    },
    /// Replay simulation using existing input events and plan
    Replay {
        #[arg(long)]
        resolved_plan: PathBuf,
        #[arg(long)]
        input_events: PathBuf,
        #[arg(long)]
        manifest: PathBuf,
        #[arg(long)]
        output: PathBuf,
        #[arg(long)]
        cache_dir: Option<PathBuf>,
    },
}

fn emit_event(event_type: &str, stage: &str, progress_pct: f64, details: serde_json::Value) {
    let msg = json!({
        "type": event_type,
        "stage": stage,
        "progress_pct": progress_pct,
        "details": details
    });
    println!("{}", msg);
    let _ = std::io::stdout().flush();
}

fn compute_sha256(path: &Path) -> Result<String, Box<dyn std::error::Error>> {
    let mut file = File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buffer = [0u8; 65536];
    loop {
        let count = std::io::Read::read(&mut file, &mut buffer)?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    Ok(hex::encode(hasher.finalize()))
}

fn resolve_graph(
    manifest_path: &Path,
    cache_dir: Option<&Path>,
) -> Result<ConnectomeGraph, Box<dyn std::error::Error>> {
    let manifest = DatasetManifest::load_from_file(manifest_path)?;
    let comp_file = manifest.files.get("completeness").ok_or("Missing completeness in manifest")?;
    let conn_file = manifest.files.get("connectivity").ok_or("Missing connectivity in manifest")?;

    let base_dir = manifest_path.parent().unwrap_or(Path::new("."));
    let comp_path = base_dir.join(&comp_file.filename);
    let conn_path = base_dir.join(&conn_file.filename);

    let default_cache_dir = base_dir.join("cache");
    let cache_dir_ref = cache_dir.unwrap_or(&default_cache_dir);
    let cache_file = cache_dir_ref.join("flywire_630_csr.bin");

    if let Some(graph) = ConnectomeGraph::load_from_cache(&cache_file, &comp_file.sha256, &conn_file.sha256)? {
        return Ok(graph);
    }

    emit_event("progress", "building", 10.0, json!({"status": "Building graph from raw sources"}));
    let graph = ConnectomeGraph::build_from_sources(&comp_path, &conn_path, 0.275)?;
    let _ = graph.save_to_cache(&cache_file, &comp_file.sha256, &conn_file.sha256);
    Ok(graph)
}

fn simulate_single_condition(
    graph: &ConnectomeGraph,
    params: &LifParameters,
    coeffs: &AnalyticalCoefficients,
    events_by_tick: &HashMap<usize, Vec<(usize, i64)>>,
    silenced_indices: &HashSet<usize>,
    stimulated_indices: &[usize],
    duration_ms: f64,
    condition_name: &str,
    trial: usize,
) -> (Vec<OutputSpike>, usize) {
    let total_ticks = (duration_ms / params.dt_ms).round() as usize;
    let mut state = NetworkState::new(graph.num_neurons, params, coeffs);
    state.set_poisson_targets(stimulated_indices);

    let mut spikes = Vec::new();
    let mut spiked_this_tick = Vec::with_capacity(256);

    for t in 0..total_ticks {
        // Stage 1: groups (State Update)
        for i in 0..graph.num_neurons {
            let last = state.last_spike_tick[i];
            let rfc = state.rfc_ticks[i];
            if (t as i64 - last) >= rfc {
                let g_val = state.g[i];
                state.g[i] = g_val * coeffs.c_g;
                state.v[i] = params.v_0_mv + (state.v[i] - params.v_0_mv) * coeffs.c_v + g_val * coeffs.c_vg;
            }
        }

        // Stage 2: thresholds (Spike detection)
        spiked_this_tick.clear();
        let delivery_slot = (t + coeffs.delay_ticks) % state.ring_len;
        for i in 0..graph.num_neurons {
            let last = state.last_spike_tick[i];
            let rfc = state.rfc_ticks[i];
            if (t as i64 - last) >= rfc && state.v[i] > params.v_th_mv {
                spiked_this_tick.push(i);
                state.last_spike_tick[i] = t as i64;
                spikes.push(OutputSpike {
                    condition: condition_name.to_string(),
                    trial,
                    root_id: graph.index_to_id[i].clone(),
                    spike_time_ms: t as f64 * params.dt_ms,
                });

                // Silence outgoing synapses if presynaptic neuron is silenced
                if !silenced_indices.contains(&i) {
                    for (target_j, weight) in graph.outgoing_edges(i) {
                        state.ring_buffer[delivery_slot][target_j] += weight;
                    }
                }
            }
        }

        // Stage 3: synapses (Incoming delayed synaptic inputs & external stimulus)
        let current_slot = t % state.ring_len;
        for i in 0..graph.num_neurons {
            let incoming_w = state.ring_buffer[current_slot][i];
            if incoming_w != 0.0 {
                state.g[i] += incoming_w;
                state.ring_buffer[current_slot][i] = 0.0;
            }
        }
        if let Some(events) = events_by_tick.get(&t) {
            for &(target_idx, mult) in events {
                state.v[target_idx] += (mult as f64) * coeffs.stimulus_step_mv;
            }
        }

        // Stage 4: resets (Spike Resetter)
        for &i in &spiked_this_tick {
            state.v[i] = params.v_rst_mv;
            state.g[i] = 0.0;
        }
    }

    let spike_count = spikes.len();
    (spikes, spike_count)
}

fn generate_report_markdown(
    plan: &ResolvedPlan,
    rates_a: &HashMap<String, f64>,
    rates_b: Option<&HashMap<String, f64>>,
    spikes_count_a: usize,
    spikes_count_b: Option<usize>,
    wall_sec: f64,
) -> String {
    let mut md = String::new();
    md.push_str("# Experiment Execution Report\n\n");
    md.push_str("> The results describe neural activity in the selected computational model under the specified parameters.\n\n");
    md.push_str("## 1. Parameters\n\n");
    md.push_str(&format!("- **Experiment Type**: `{}`\n", plan.experiment_type));
    md.push_str(&format!("- **Dataset**: `{}`\n", plan.dataset_id));
    md.push_str(&format!("- **Duration**: `{:.1} ms`\n", plan.duration_ms));
    md.push_str(&format!("- **Repeats**: `{}`\n", plan.repeats));
    md.push_str(&format!("- **Base Seed**: `{}`\n", plan.base_seed));
    md.push_str(&format!("- **Total Wall Time**: `{:.2} s`\n\n", wall_sec));

    md.push_str("## 2. Readout Neurons Activity\n\n");
    md.push_str("| Root ID | Condition A (Hz) | Condition B (Hz) | Delta (Hz) | Status |\n");
    md.push_str("|---|---|---|---|---|\n");

    for r_id in &plan.readout_neuron_ids {
        let r_a = rates_a.get(r_id).copied().unwrap_or(0.0);
        if let Some(rb_map) = rates_b {
            let r_b = rb_map.get(r_id).copied().unwrap_or(0.0);
            let delta = r_b - r_a;
            md.push_str(&format!(
                "| `{}` | {:.2} | {:.2} | {:.2} | Measured |\n",
                r_id, r_a, r_b, delta
            ));
        } else {
            md.push_str(&format!(
                "| `{}` | {:.2} | N/A | N/A | Measured |\n",
                r_id, r_a
            ));
        }
    }

    md.push_str("\n## 3. Network Summary\n\n");
    md.push_str(&format!("- Condition A Total Spikes: `{}`\n", spikes_count_a));
    if let Some(cnt_b) = spikes_count_b {
        md.push_str(&format!("- Condition B Total Spikes: `{}`\n", cnt_b));
    }
    md
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();

    match cli.command {
        Commands::Inspect { dataset_manifest } => {
            let manifest = DatasetManifest::load_from_file(&dataset_manifest)?;
            let base_dir = dataset_manifest.parent().unwrap_or(Path::new("."));

            let mut files_status = Vec::new();
            let mut all_valid = true;

            for (key, info) in &manifest.files {
                let p = base_dir.join(&info.filename);
                let exists = p.exists();
                let mut hash_matches = false;
                let mut computed_hash = String::new();

                if exists {
                    if let Ok(h) = compute_sha256(&p) {
                        hash_matches = h == info.sha256;
                        computed_hash = h;
                    }
                }
                if !exists || !hash_matches {
                    all_valid = false;
                }

                files_status.push(json!({
                    "name": key,
                    "filename": info.filename,
                    "exists": exists,
                    "expected_sha256": info.sha256,
                    "computed_sha256": computed_hash,
                    "valid": hash_matches
                }));
            }

            let result = json!({
                "status": if all_valid { "valid" } else { "invalid" },
                "dataset_id": manifest.dataset_id,
                "neuron_count": manifest.neuron_count,
                "connection_count": manifest.connection_count,
                "files": files_status
            });
            println!("{}", serde_json::to_string_pretty(&result)?);
        }

        Commands::Prepare {
            dataset_manifest,
            output,
        } => {
            let t0 = Instant::now();
            emit_event("progress", "loading", 10.0, json!({"msg": "Reading dataset files"}));
            let manifest = DatasetManifest::load_from_file(&dataset_manifest)?;
            let base_dir = dataset_manifest.parent().unwrap_or(Path::new("."));

            let comp_info = manifest.files.get("completeness").ok_or("Missing completeness")?;
            let conn_info = manifest.files.get("connectivity").ok_or("Missing connectivity")?;

            let comp_path = base_dir.join(&comp_info.filename);
            let conn_path = base_dir.join(&conn_info.filename);

            emit_event("progress", "building", 30.0, json!({"msg": "Building CSR graph"}));
            let graph = ConnectomeGraph::build_from_sources(&comp_path, &conn_path, 0.275)?;

            let cache_path = output.join("flywire_630_csr.bin");
            emit_event("progress", "saving", 80.0, json!({"msg": "Saving binary cache"}));
            graph.save_to_cache(&cache_path, &comp_info.sha256, &conn_info.sha256)?;

            let elapsed = t0.elapsed().as_secs_f64();
            emit_event(
                "result",
                "ready",
                100.0,
                json!({
                    "status": "prepared",
                    "cache_file": cache_path.to_string_lossy(),
                    "num_neurons": graph.num_neurons,
                    "num_connections": graph.num_edges,
                    "elapsed_seconds": elapsed
                }),
            );
        }

        Commands::Run {
            resolved_plan,
            output,
            cache_dir,
            manifest,
        } => {
            let t_total_start = Instant::now();
            std::fs::create_dir_all(&output)?;

            emit_event("progress", "loading", 5.0, json!({"msg": "Loading resolved plan"}));
            let plan = ResolvedPlan::load_from_file(&resolved_plan)?;

            let manifest_path = if let Some(ref m) = manifest {
                m.clone()
            } else if let Some(ref c) = cache_dir {
                let candidate = c.join("../dataset_manifest.json");
                if candidate.exists() {
                    candidate
                } else {
                    PathBuf::from("data/dataset_manifest.json")
                }
            } else {
                PathBuf::from("data/dataset_manifest.json")
            };
            emit_event("progress", "loading", 15.0, json!({"msg": "Loading connectome graph"}));
            let graph = resolve_graph(&manifest_path, cache_dir.as_deref())?;

            let params = LifParameters::default();
            let coeffs = AnalyticalCoefficients::new(&params);

            // Collect stimulated IDs
            let mut stimulated_ids = Vec::new();
            for act in &plan.activation {
                stimulated_ids.extend(act.neuron_ids.clone());
            }
            stimulated_ids.sort();
            stimulated_ids.dedup();

            let stimulated_indices: Vec<usize> = stimulated_ids
                .iter()
                .filter_map(|id| graph.id_to_index.get(id).copied())
                .collect();

            // Collect silenced indices
            let silenced_indices: HashSet<usize> = plan
                .silencing_neuron_ids
                .iter()
                .filter_map(|id| graph.id_to_index.get(id).copied())
                .collect();

            // Generate deterministic input events for trial 0..repeats
            emit_event("progress", "simulating", 25.0, json!({"msg": "Generating stimulus events"}));
            let mut all_events = Vec::new();
            for trial in 0..plan.repeats {
                let trial_seed = plan.base_seed + trial as u64;
                let rate = plan.activation.first().map(|a| a.rate_hz).unwrap_or(50.0);
                let events = generate_deterministic_events(
                    &stimulated_ids,
                    plan.duration_ms,
                    rate,
                    trial_seed,
                    params.dt_ms,
                    trial,
                );
                all_events.extend(events);
            }
            save_events_to_parquet(&all_events, &output.join("input_events.parquet"))?;

            let mut all_spikes = Vec::new();
            let mut total_spikes_a = 0;
            let mut total_spikes_b: Option<usize> = None;

            // Run Condition A
            for trial in 0..plan.repeats {
                emit_event(
                    "progress",
                    "simulating",
                    30.0 + (trial as f64 / plan.repeats as f64) * 30.0,
                    json!({"condition": "A", "trial": trial}),
                );
                let events_map = index_events_by_tick(&all_events, &graph.id_to_index, trial);
                let (spikes_a, cnt) = simulate_single_condition(
                    &graph,
                    &params,
                    &coeffs,
                    &events_map,
                    &HashSet::new(), // Condition A has no silencing
                    &stimulated_indices,
                    plan.duration_ms,
                    "A",
                    trial,
                );
                total_spikes_a += cnt;
                all_spikes.extend(spikes_a);
            }

            // Run Condition B if compare_silencing
            if plan.experiment_type == "compare_silencing" {
                let mut b_count = 0;
                for trial in 0..plan.repeats {
                    emit_event(
                        "progress",
                        "simulating",
                        65.0 + (trial as f64 / plan.repeats as f64) * 25.0,
                        json!({"condition": "B", "trial": trial}),
                    );
                    let events_map = index_events_by_tick(&all_events, &graph.id_to_index, trial);
                    let (spikes_b, cnt) = simulate_single_condition(
                        &graph,
                        &params,
                        &coeffs,
                        &events_map,
                        &silenced_indices,
                        &stimulated_indices,
                        plan.duration_ms,
                        "B",
                        trial,
                    );
                    b_count += cnt;
                    all_spikes.extend(spikes_b);
                }
                total_spikes_b = Some(b_count);
            }

            emit_event("progress", "aggregating", 90.0, json!({"msg": "Aggregating outputs"}));

            // Save spikes parquet
            save_spikes_parquet(&all_spikes, &output.join("spikes.parquet"))?;

            // Compute rates
            let duration_sec = plan.duration_ms / 1000.0;
            let mut counts_a: HashMap<String, usize> = HashMap::new();
            let mut counts_b: HashMap<String, usize> = HashMap::new();

            for s in &all_spikes {
                if s.condition == "A" {
                    *counts_a.entry(s.root_id.clone()).or_insert(0) += 1;
                } else if s.condition == "B" {
                    *counts_b.entry(s.root_id.clone()).or_insert(0) += 1;
                }
            }

            let mut rate_records = Vec::new();
            let readout_set: HashSet<&str> = plan.readout_neuron_ids.iter().map(|s| s.as_str()).collect();

            // Rates Condition A
            for r_id in &plan.readout_neuron_ids {
                let c = counts_a.get(r_id).copied().unwrap_or(0);
                rate_records.push(NeuronRate {
                    condition: "A".to_string(),
                    trial: 0,
                    root_id: r_id.clone(),
                    spike_count: c,
                    rate_hz: (c as f64 / (plan.repeats as f64 * duration_sec)),
                    is_readout: true,
                });
            }
            for (r_id, c) in &counts_a {
                if !readout_set.contains(r_id.as_str()) {
                    rate_records.push(NeuronRate {
                        condition: "A".to_string(),
                        trial: 0,
                        root_id: r_id.clone(),
                        spike_count: *c,
                        rate_hz: (*c as f64 / (plan.repeats as f64 * duration_sec)),
                        is_readout: false,
                    });
                }
            }

            // Rates Condition B if applicable
            if plan.experiment_type == "compare_silencing" {
                for r_id in &plan.readout_neuron_ids {
                    let c = counts_b.get(r_id).copied().unwrap_or(0);
                    rate_records.push(NeuronRate {
                        condition: "B".to_string(),
                        trial: 0,
                        root_id: r_id.clone(),
                        spike_count: c,
                        rate_hz: (c as f64 / (plan.repeats as f64 * duration_sec)),
                        is_readout: true,
                    });
                }
                for (r_id, c) in &counts_b {
                    if !readout_set.contains(r_id.as_str()) {
                        rate_records.push(NeuronRate {
                            condition: "B".to_string(),
                            trial: 0,
                            root_id: r_id.clone(),
                            spike_count: *c,
                            rate_hz: (*c as f64 / (plan.repeats as f64 * duration_sec)),
                            is_readout: false,
                        });
                    }
                }
            }

            save_rates_csv(&rate_records, &output.join("rates.csv"))?;

            // Compute comparison
            let mut rates_map_a = HashMap::new();
            let mut rates_map_b = HashMap::new();
            for r in &rate_records {
                if r.condition == "A" {
                    rates_map_a.insert(r.root_id.clone(), r.rate_hz);
                } else if r.condition == "B" {
                    rates_map_b.insert(r.root_id.clone(), r.rate_hz);
                }
            }

            let mut comp_rows = Vec::new();
            let all_recorded_ids: HashSet<String> = rates_map_a
                .keys()
                .chain(rates_map_b.keys())
                .cloned()
                .collect();

            for r_id in all_recorded_ids {
                let r_a = rates_map_a.get(&r_id).copied().unwrap_or(0.0);
                let r_b = rates_map_b.get(&r_id).copied().unwrap_or(0.0);
                let delta = r_b - r_a;
                let rel = if r_a > 0.0 { Some(delta / r_a) } else { None };
                comp_rows.push(ComparisonRow {
                    root_id: r_id.clone(),
                    is_readout: readout_set.contains(r_id.as_str()),
                    rate_A_hz: r_a,
                    rate_B_hz: r_b,
                    delta_hz: delta,
                    relative_change: rel,
                });
            }
            comp_rows.sort_by(|a, b| {
                b.is_readout
                    .cmp(&a.is_readout)
                    .then_with(|| b.rate_A_hz.partial_cmp(&a.rate_A_hz).unwrap_or(std::cmp::Ordering::Equal))
            });
            save_comparison_csv(&comp_rows, &output.join("comparison.csv"))?;

            let total_wall_s = t_total_start.elapsed().as_secs_f64();

            // Summary JSON
            let readout_summary: Vec<serde_json::Value> = plan
                .readout_neuron_ids
                .iter()
                .map(|r_id| {
                    let r_a = rates_map_a.get(r_id).copied().unwrap_or(0.0);
                    let r_b = rates_map_b.get(r_id).copied().unwrap_or(0.0);
                    json!({
                        "root_id": r_id,
                        "rate_A_hz": r_a,
                        "rate_B_hz": r_b,
                        "delta_hz": r_b - r_a
                    })
                })
                .collect();

            let summary = json!({
                "dataset_id": plan.dataset_id,
                "model_id": plan.model_id,
                "experiment_type": plan.experiment_type,
                "duration_ms": plan.duration_ms,
                "repeats": plan.repeats,
                "readout_neurons": plan.readout_neuron_ids,
                "readout_summary": readout_summary,
                "active_neurons_count_A": rates_map_a.values().filter(|&&v| v > 0.0).count(),
                "active_neurons_count_B": rates_map_b.values().filter(|&&v| v > 0.0).count(),
                "total_spikes_A": total_spikes_a,
                "total_spikes_B": total_spikes_b
            });
            std::fs::write(output.join("summary.json"), serde_json::to_string_pretty(&summary)?)?;

            // Metrics JSON
            let metrics = json!({
                "engine": "flysim_rust",
                "wall_seconds": total_wall_s,
                "duration_ms": plan.duration_ms,
                "repeats": plan.repeats,
                "total_spikes_A": total_spikes_a,
                "total_spikes_B": total_spikes_b
            });
            std::fs::write(output.join("metrics.json"), serde_json::to_string_pretty(&metrics)?)?;

            // Report Markdown
            let report_md = generate_report_markdown(
                &plan,
                &rates_map_a,
                if plan.experiment_type == "compare_silencing" { Some(&rates_map_b) } else { None },
                total_spikes_a,
                total_spikes_b,
                total_wall_s,
            );
            std::fs::write(output.join("report.md"), report_md)?;

            // Replay script
            let replay_sh = "#!/usr/bin/env bash\nset -euo pipefail\nflysim replay --resolved-plan resolved_plan.json --input-events input_events.parquet --manifest manifest.json --output replayed_output\n";
            std::fs::write(output.join("replay.sh"), replay_sh)?;

            // Checksums
            let files_to_hash = [
                "spikes.parquet",
                "input_events.parquet",
                "rates.csv",
                "comparison.csv",
                "summary.json",
                "metrics.json",
                "report.md",
                "replay.sh",
            ];
            generate_checksums(&output, &files_to_hash)?;

            emit_event(
                "result",
                "succeeded",
                100.0,
                json!({
                    "status": "completed",
                    "total_spikes_A": total_spikes_a,
                    "total_spikes_B": total_spikes_b,
                    "wall_seconds": total_wall_s
                }),
            );
        }

        Commands::Replay {
            resolved_plan,
            input_events,
            manifest,
            output,
            cache_dir,
        } => {
            let t_total_start = Instant::now();
            std::fs::create_dir_all(&output)?;

            let plan = ResolvedPlan::load_from_file(&resolved_plan)?;
            let graph = resolve_graph(&manifest, cache_dir.as_deref())?;

            let params = LifParameters::default();
            let coeffs = AnalyticalCoefficients::new(&params);

            let all_events = load_events_from_parquet(&input_events)?;

            let mut stimulated_ids = Vec::new();
            for act in &plan.activation {
                stimulated_ids.extend(act.neuron_ids.clone());
            }
            stimulated_ids.sort();
            stimulated_ids.dedup();
            let stimulated_indices: Vec<usize> = stimulated_ids
                .iter()
                .filter_map(|id| graph.id_to_index.get(id).copied())
                .collect();

            let silenced_indices: HashSet<usize> = plan
                .silencing_neuron_ids
                .iter()
                .filter_map(|id| graph.id_to_index.get(id).copied())
                .collect();

            let mut all_spikes = Vec::new();
            let mut total_spikes_a = 0;
            let mut total_spikes_b: Option<usize> = None;

            for trial in 0..plan.repeats {
                let events_map = index_events_by_tick(&all_events, &graph.id_to_index, trial);
                let (spikes_a, cnt) = simulate_single_condition(
                    &graph,
                    &params,
                    &coeffs,
                    &events_map,
                    &HashSet::new(),
                    &stimulated_indices,
                    plan.duration_ms,
                    "A",
                    trial,
                );
                total_spikes_a += cnt;
                all_spikes.extend(spikes_a);
            }

            if plan.experiment_type == "compare_silencing" {
                let mut b_count = 0;
                for trial in 0..plan.repeats {
                    let events_map = index_events_by_tick(&all_events, &graph.id_to_index, trial);
                    let (spikes_b, cnt) = simulate_single_condition(
                        &graph,
                        &params,
                        &coeffs,
                        &events_map,
                        &silenced_indices,
                        &stimulated_indices,
                        plan.duration_ms,
                        "B",
                        trial,
                    );
                    b_count += cnt;
                    all_spikes.extend(spikes_b);
                }
                total_spikes_b = Some(b_count);
            }

            save_spikes_parquet(&all_spikes, &output.join("spikes.parquet"))?;
            let total_wall_s = t_total_start.elapsed().as_secs_f64();

            let result = json!({
                "status": "replayed",
                "total_spikes_A": total_spikes_a,
                "total_spikes_B": total_spikes_b,
                "wall_seconds": total_wall_s
            });
            let pretty_json = serde_json::to_string_pretty(&result)?;
            std::fs::write(output.join("summary.json"), &pretty_json)?;
            println!("{}", pretty_json);
        }
    }

    Ok(())
}
