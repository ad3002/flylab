use std::fs::File;
use std::io::Write;
use std::path::Path;
use std::sync::Arc;

use arrow::array::{Array, ArrayRef, Float64Array, Int64Array, StringArray};
use arrow::datatypes::{DataType, Field, Schema};
use arrow::record_batch::RecordBatch;
use parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder;
use parquet::arrow::ArrowWriter;
use parquet::file::properties::WriterProperties;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OutputSpike {
    pub condition: String,
    pub trial: usize,
    pub root_id: String,
    pub spike_time_ms: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NeuronRate {
    pub condition: String,
    pub trial: usize,
    pub root_id: String,
    pub spike_count: usize,
    pub rate_hz: f64,
    pub is_readout: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[allow(non_snake_case)]
pub struct ComparisonRow {
    pub root_id: String,
    pub is_readout: bool,
    pub rate_A_hz: f64,
    pub rate_B_hz: f64,
    pub delta_hz: f64,
    pub relative_change: Option<f64>,
}

pub fn save_spikes_parquet(
    spikes: &[OutputSpike],
    path: &Path,
) -> Result<(), Box<dyn std::error::Error>> {
    let schema = Arc::new(Schema::new(vec![
        Field::new("condition", DataType::Utf8, false),
        Field::new("trial", DataType::Int64, false),
        Field::new("root_id", DataType::Utf8, false),
        Field::new("spike_time_ms", DataType::Float64, false),
    ]));

    let conditions: Vec<&str> = spikes.iter().map(|s| s.condition.as_str()).collect();
    let trials: Vec<i64> = spikes.iter().map(|s| s.trial as i64).collect();
    let root_ids: Vec<&str> = spikes.iter().map(|s| s.root_id.as_str()).collect();
    let times: Vec<f64> = spikes.iter().map(|s| s.spike_time_ms).collect();

    let cond_arr = Arc::new(StringArray::from(conditions)) as ArrayRef;
    let trial_arr = Arc::new(Int64Array::from(trials)) as ArrayRef;
    let root_arr = Arc::new(StringArray::from(root_ids)) as ArrayRef;
    let time_arr = Arc::new(Float64Array::from(times)) as ArrayRef;

    let batch = RecordBatch::try_new(
        schema.clone(),
        vec![cond_arr, trial_arr, root_arr, time_arr],
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

/// Reads a spikes.parquet written by `save_spikes_parquet`. A missing column, a wrong column
/// type, a null value or a negative trial is an error naming the problem (never skipped).
pub fn load_spikes_parquet(path: &Path) -> Result<Vec<OutputSpike>, Box<dyn std::error::Error>> {
    let file = File::open(path)?;
    let reader = ParquetRecordBatchReaderBuilder::try_new(file)?.build()?;
    let mut spikes = Vec::new();
    for batch in reader {
        let batch = batch?;
        let col = |name: &str| batch.column_by_name(name).ok_or_else(|| format!("spikes parquet lacks column {name}"));
        let cond = col("condition")?
            .as_any()
            .downcast_ref::<StringArray>()
            .ok_or("spikes parquet column condition is not a string column")?;
        let trial = col("trial")?
            .as_any()
            .downcast_ref::<Int64Array>()
            .ok_or("spikes parquet column trial is not an int64 column")?;
        let root = col("root_id")?
            .as_any()
            .downcast_ref::<StringArray>()
            .ok_or("spikes parquet column root_id is not a string column")?;
        let time = col("spike_time_ms")?
            .as_any()
            .downcast_ref::<Float64Array>()
            .ok_or("spikes parquet column spike_time_ms is not a float64 column")?;
        for i in 0..batch.num_rows() {
            if cond.is_null(i) || trial.is_null(i) || root.is_null(i) || time.is_null(i) {
                return Err(format!("spikes parquet row {} has a null value", spikes.len()).into());
            }
            let t = trial.value(i);
            if t < 0 {
                return Err(format!("spikes parquet row {} has a negative trial ({t})", spikes.len()).into());
            }
            spikes.push(OutputSpike {
                condition: cond.value(i).to_string(),
                trial: t as usize,
                root_id: root.value(i).to_string(),
                spike_time_ms: time.value(i),
            });
        }
    }
    Ok(spikes)
}

pub fn save_rates_csv(
    rates: &[NeuronRate],
    path: &Path,
) -> Result<(), Box<dyn std::error::Error>> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut file = File::create(path)?;
    writeln!(
        file,
        "condition,trial,root_id,spike_count,rate_hz,is_readout"
    )?;
    for r in rates {
        writeln!(
            file,
            "{},{},{},{},{:.4},{}",
            r.condition, r.trial, r.root_id, r.spike_count, r.rate_hz, r.is_readout
        )?;
    }
    Ok(())
}

pub fn save_comparison_csv(
    comp: &[ComparisonRow],
    path: &Path,
) -> Result<(), Box<dyn std::error::Error>> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut file = File::create(path)?;
    writeln!(
        file,
        "root_id,is_readout,rate_A_hz,rate_B_hz,delta_hz,relative_change"
    )?;
    for c in comp {
        let rel_str = match c.relative_change {
            Some(v) => format!("{:.6}", v),
            None => "".to_string(),
        };
        writeln!(
            file,
            "{},{},{:.4},{:.4},{:.4},{}",
            c.root_id, c.is_readout, c.rate_A_hz, c.rate_B_hz, c.delta_hz, rel_str
        )?;
    }
    Ok(())
}

pub fn generate_checksums(
    dir: &Path,
    filenames: &[&str],
) -> Result<String, Box<dyn std::error::Error>> {
    let mut out = String::new();
    for fname in filenames {
        let p = dir.join(fname);
        if p.exists() {
            let data = std::fs::read(&p)?;
            let mut hasher = Sha256::new();
            hasher.update(&data);
            let hash = hex::encode(hasher.finalize());
            out.push_str(&format!("{}  {}\n", hash, fname));
        }
    }
    let checksum_path = dir.join("checksums.sha256");
    std::fs::write(checksum_path, &out)?;
    Ok(out)
}
