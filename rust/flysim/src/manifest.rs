use std::collections::HashMap;
use std::fs::File;
use std::path::Path;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DatasetFileInfo {
    pub path: String,
    pub filename: String,
    pub size_bytes: usize,
    pub sha256: String,
    pub rows: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DatasetManifest {
    pub schema_version: String,
    pub dataset_id: String,
    pub name: String,
    pub upstream_repository: String,
    pub upstream_commit: String,
    pub neuron_count: usize,
    pub connection_count: usize,
    pub files: HashMap<String, DatasetFileInfo>,
}

impl DatasetManifest {
    pub fn load_from_file(path: &Path) -> Result<Self, Box<dyn std::error::Error>> {
        let file = File::open(path)?;
        let manifest: Self = serde_json::from_reader(file)?;
        Ok(manifest)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResolvedActivation {
    pub neuron_ids: Vec<String>,
    pub rate_hz: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResolvedPlan {
    pub schema_version: String,
    pub plan_id: Option<String>,
    pub plan_hash: Option<String>,
    pub dataset_id: String,
    pub model_id: String,
    pub experiment_type: String, // "single" | "compare_silencing"
    pub activation: Vec<ResolvedActivation>,
    #[serde(default)]
    pub silencing_neuron_ids: Vec<String>,
    pub readout_neuron_ids: Vec<String>,
    pub duration_ms: f64,
    pub repeats: usize,
    pub base_seed: u64,
    pub report_language: String,
    pub dt_ms: Option<f64>,
}

impl ResolvedPlan {
    pub fn load_from_file(path: &Path) -> Result<Self, Box<dyn std::error::Error>> {
        let file = File::open(path)?;
        let plan: Self = serde_json::from_reader(file)?;
        Ok(plan)
    }
}
