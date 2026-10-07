use std::collections::{BTreeMap, HashMap};
use std::fs::File;
use std::path::Path;
use std::sync::Arc;

use arrow::array::{Array, ArrayRef, Int64Array, StringArray};
use arrow::datatypes::{DataType, Field, Schema};
use arrow::record_batch::RecordBatch;
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ArrowWriter;
use parquet::file::properties::WriterProperties;
use rand::Rng;
use rand::SeedableRng;
use rand_chacha::ChaCha8Rng;
use serde::{Deserialize, Serialize};

use crate::manifest::ResolvedActivation;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InputEvent {
    pub trial: usize,
    pub root_id: String,
    pub timestep: usize,
    pub multiplicity: i64,
}

/// Per-neuron stimulation rates from all activation sets, sorted by root id.
///
/// Each set keeps its own `rate_hz`. A neuron listed in two sets is an error (its rate would be
/// ambiguous), as are an empty activation list and a non-finite or negative rate.
pub fn stimulation_rates(activation: &[ResolvedActivation]) -> Result<Vec<(String, f64)>, String> {
    if activation.is_empty() {
        return Err("plan has no activation sets".to_string());
    }
    let mut owner: BTreeMap<String, (usize, f64)> = BTreeMap::new();
    for (idx, act) in activation.iter().enumerate() {
        if !act.rate_hz.is_finite() || act.rate_hz < 0.0 {
            return Err(format!("activation[{idx}].rate_hz is invalid ({})", act.rate_hz));
        }
        for id in &act.neuron_ids {
            if let Some((prev, _)) = owner.get(id) {
                if *prev != idx {
                    return Err(format!(
                        "neuron {id} is in both activation[{prev}] and activation[{idx}]; each neuron can be driven at only one rate"
                    ));
                }
                continue;
            }
            owner.insert(id.clone(), (idx, act.rate_hz));
        }
    }
    Ok(owner.into_iter().map(|(id, (_, rate))| (id, rate)).collect())
}

/// Generates deterministic Bernoulli events matching PoissonInput(N=1, p = rate * dt), with a
/// per-neuron rate. Neurons are drawn in the given order from one ChaCha8 stream, so a single
/// rate over sorted ids reproduces `generate_deterministic_events` bit for bit.
pub fn generate_deterministic_events_with_rates(
    stimulated: &[(String, f64)],
    duration_ms: f64,
    seed: u64,
    dt_ms: f64,
    trial: usize,
) -> Vec<InputEvent> {
    let mut rng = ChaCha8Rng::seed_from_u64(seed);
    let total_ticks = (duration_ms / dt_ms).round() as usize;

    let mut events = Vec::new();
    for (root_id, rate_hz) in stimulated {
        let p = rate_hz * (dt_ms / 1000.0);
        for tick in 0..total_ticks {
            let sample: f64 = rng.gen();
            if sample < p {
                events.push(InputEvent {
                    trial,
                    root_id: root_id.clone(),
                    timestep: tick,
                    multiplicity: 1,
                });
            }
        }
    }

    events.sort_by(|a, b| a.timestep.cmp(&b.timestep).then_with(|| a.root_id.cmp(&b.root_id)));
    events
}

/// Generates deterministic Bernoulli events matching PoissonInput(N=1, p = rate * dt), one rate
/// for every neuron.
pub fn generate_deterministic_events(
    stimulated_ids: &[String],
    duration_ms: f64,
    rate_hz: f64,
    seed: u64,
    dt_ms: f64,
    trial: usize,
) -> Vec<InputEvent> {
    let stimulated: Vec<(String, f64)> = stimulated_ids.iter().map(|id| (id.clone(), rate_hz)).collect();
    generate_deterministic_events_with_rates(&stimulated, duration_ms, seed, dt_ms, trial)
}

/// Saves events to Parquet format.
pub fn save_events_to_parquet(
    events: &[InputEvent],
    path: &Path,
) -> Result<(), Box<dyn std::error::Error>> {
    let schema = Arc::new(Schema::new(vec![
        Field::new("trial", DataType::Int64, false),
        Field::new("root_id", DataType::Utf8, false),
        Field::new("timestep", DataType::Int64, false),
        Field::new("multiplicity", DataType::Int64, false),
    ]));

    let trials: Vec<i64> = events.iter().map(|e| e.trial as i64).collect();
    let root_ids: Vec<&str> = events.iter().map(|e| e.root_id.as_str()).collect();
    let timesteps: Vec<i64> = events.iter().map(|e| e.timestep as i64).collect();
    let multiplicities: Vec<i64> = events.iter().map(|e| e.multiplicity).collect();

    let trial_array = Arc::new(Int64Array::from(trials)) as ArrayRef;
    let root_id_array = Arc::new(StringArray::from(root_ids)) as ArrayRef;
    let timestep_array = Arc::new(Int64Array::from(timesteps)) as ArrayRef;
    let mult_array = Arc::new(Int64Array::from(multiplicities)) as ArrayRef;

    let batch = RecordBatch::try_new(
        schema.clone(),
        vec![trial_array, root_id_array, timestep_array, mult_array],
    )?;

    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let file = File::create(path)?;
    let props = WriterProperties::builder().build();
    let mut writer = ArrowWriter::try_new(file, schema, Some(props))?;
    writer.write(&batch)?;
    writer.close()?;

    Ok(())
}

/// Reads events from Parquet format.
pub fn load_events_from_parquet(
    path: &Path,
) -> Result<Vec<InputEvent>, Box<dyn std::error::Error>> {
    let file = File::open(path)?;
    let builder = ParquetRecordBatchReaderBuilder::try_new(file)?;
    let reader = builder.build()?;

    let mut events = Vec::new();

    for batch_result in reader {
        let batch = batch_result?;
        let trial_array = batch
            .column_by_name("trial")
            .ok_or("Missing trial column")?
            .as_any()
            .downcast_ref::<Int64Array>()
            .ok_or("Invalid trial array")?;

        let root_id_array = batch
            .column_by_name("root_id")
            .ok_or("Missing root_id column")?
            .as_any()
            .downcast_ref::<StringArray>()
            .ok_or("Invalid root_id array")?;

        let timestep_array = batch
            .column_by_name("timestep")
            .ok_or("Missing timestep column")?
            .as_any()
            .downcast_ref::<Int64Array>()
            .ok_or("Invalid timestep array")?;

        let mult_array = batch
            .column_by_name("multiplicity")
            .ok_or("Missing multiplicity column")?
            .as_any()
            .downcast_ref::<Int64Array>()
            .ok_or("Invalid multiplicity array")?;

        for i in 0..batch.num_rows() {
            events.push(InputEvent {
                trial: trial_array.value(i) as usize,
                root_id: root_id_array.value(i).to_string(),
                timestep: timestep_array.value(i) as usize,
                multiplicity: mult_array.value(i),
            });
        }
    }

    Ok(events)
}

/// Index events into tick lookup: tick -> Vec<(neuron_index, multiplicity)>.
///
/// An event whose root id is not in the graph is an error naming the ids: dropping it would
/// silently simulate a different stimulus than the one recorded (e.g. a replay against another
/// connectome version).
pub fn index_events_by_tick(
    events: &[InputEvent],
    id_to_index: &HashMap<String, usize>,
    target_trial: usize,
) -> Result<HashMap<usize, Vec<(usize, i64)>>, String> {
    let mut map = HashMap::new();
    let mut unknown_events = 0usize;
    let mut unknown_ids: Vec<&str> = Vec::new();
    for e in events {
        if e.trial == target_trial {
            match id_to_index.get(&e.root_id) {
                Some(&idx) => map
                    .entry(e.timestep)
                    .or_insert_with(Vec::new)
                    .push((idx, e.multiplicity)),
                None => {
                    unknown_events += 1;
                    if !unknown_ids.contains(&e.root_id.as_str()) {
                        unknown_ids.push(e.root_id.as_str());
                    }
                }
            }
        }
    }
    if unknown_events > 0 {
        let shown: Vec<&str> = unknown_ids.iter().take(5).copied().collect();
        return Err(format!(
            "{} input event(s) of trial {} reference {} root id(s) that are not in the connectome graph: {}{}",
            unknown_events,
            target_trial,
            unknown_ids.len(),
            shown.join(", "),
            if unknown_ids.len() > 5 { ", ..." } else { "" }
        ));
    }
    Ok(map)
}
