use std::collections::HashMap;
use std::fs::File;
use std::io::{BufRead, BufReader, BufWriter, Read, Write};
use std::path::Path;

use arrow::array::{Array, Int64Array};
use serde::{Deserialize, Serialize};

pub const CACHE_MAGIC: &[u8; 16] = b"FLYSIM_GRAPH_V1\0";

#[derive(Debug, Serialize, Deserialize)]
pub struct GraphMetadata {
    pub num_neurons: usize,
    pub num_synapses: usize,
    pub completeness_sha256: String,
    pub connectivity_sha256: String,
}

#[derive(Serialize, Deserialize)]
pub struct ConnectomeGraph {
    pub num_neurons: usize,
    pub num_edges: usize,
    pub id_to_index: HashMap<String, usize>,
    pub index_to_id: Vec<String>,
    pub row_offsets: Vec<usize>,
    pub col_indices: Vec<u32>,
    pub weights_mv: Vec<f64>,
}

impl ConnectomeGraph {
    /// Return the outgoing edges (postsynaptic target index, synaptic weight in mV) for neuron `u`.
    #[inline(always)]
    pub fn outgoing_edges(&self, u: usize) -> impl Iterator<Item = (usize, f64)> + '_ {
        let start = self.row_offsets[u];
        let end = self.row_offsets[u + 1];
        (start..end).map(move |edge_idx| {
            (
                self.col_indices[edge_idx] as usize,
                self.weights_mv[edge_idx],
            )
        })
    }

    /// Load graph from verified binary cache if hashes match, else returns None.
    pub fn load_from_cache(
        cache_path: &Path,
        expected_completeness_sha256: &str,
        expected_connectivity_sha256: &str,
    ) -> Result<Option<Self>, Box<dyn std::error::Error>> {
        if !cache_path.exists() {
            return Ok(None);
        }

        let file = File::open(cache_path)?;
        let mut reader = BufReader::new(file);

        let mut magic = [0u8; 16];
        reader.read_exact(&mut magic)?;
        if &magic != CACHE_MAGIC {
            eprintln!("[flysim] Cache magic mismatch, invalidating cache.");
            return Ok(None);
        }

        // Read stored hashes
        let mut comp_hash_bytes = [0u8; 64];
        reader.read_exact(&mut comp_hash_bytes)?;
        let comp_hash_str = std::str::from_utf8(&comp_hash_bytes)?.trim_end_matches('\0');

        let mut conn_hash_bytes = [0u8; 64];
        reader.read_exact(&mut conn_hash_bytes)?;
        let conn_hash_str = std::str::from_utf8(&conn_hash_bytes)?.trim_end_matches('\0');

        if comp_hash_str != expected_completeness_sha256 || conn_hash_str != expected_connectivity_sha256 {
            eprintln!("[flysim] Dataset checksum mismatch in cache, invalidating cache.");
            return Ok(None);
        }

        eprintln!("[flysim] Loading graph from validated binary cache: {:?}", cache_path);
        let graph: ConnectomeGraph = bincode::deserialize_from(reader)?;
        eprintln!(
            "[flysim] Loaded {} neurons and {} connections from cache.",
            graph.num_neurons, graph.num_edges
        );
        Ok(Some(graph))
    }

    /// Save graph to verified binary cache.
    pub fn save_to_cache(
        &self,
        cache_path: &Path,
        completeness_sha256: &str,
        connectivity_sha256: &str,
    ) -> Result<(), Box<dyn std::error::Error>> {
        if let Some(parent) = cache_path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let file = File::create(cache_path)?;
        let mut writer = BufWriter::new(file);

        // Write magic
        writer.write_all(CACHE_MAGIC)?;

        // Write fixed-size hash headers (64 bytes each)
        let mut comp_buf = [0u8; 64];
        let bytes_comp = completeness_sha256.as_bytes();
        comp_buf[..bytes_comp.len().min(64)].copy_from_slice(&bytes_comp[..bytes_comp.len().min(64)]);
        writer.write_all(&comp_buf)?;

        let mut conn_buf = [0u8; 64];
        let bytes_conn = connectivity_sha256.as_bytes();
        conn_buf[..bytes_conn.len().min(64)].copy_from_slice(&bytes_conn[..bytes_conn.len().min(64)]);
        writer.write_all(&conn_buf)?;

        bincode::serialize_into(writer, self)?;
        eprintln!("[flysim] Saved binary graph cache to {:?}", cache_path);
        Ok(())
    }

    /// Build graph directly from raw CSV completeness and Parquet connectivity files.
    pub fn build_from_sources(
        completeness_csv: &Path,
        connectivity_parquet: &Path,
        unit_weight_mv: f64,
    ) -> Result<Self, Box<dyn std::error::Error>> {
        eprintln!("[flysim] Parsing completeness table: {:?}", completeness_csv);
        let file = File::open(completeness_csv)?;
        let reader = BufReader::new(file);

        let mut id_to_index = HashMap::new();
        let mut index_to_id = Vec::new();

        for (line_idx, line) in reader.lines().enumerate() {
            let line = line?;
            if line_idx == 0 || line.trim().is_empty() {
                continue; // skip header
            }
            let flywire_id = line.split(',').next().unwrap_or("").trim().to_string();
            if !flywire_id.is_empty() {
                let idx = index_to_id.len();
                id_to_index.insert(flywire_id.clone(), idx);
                index_to_id.push(flywire_id);
            }
        }

        let num_neurons = index_to_id.len();
        eprintln!("[flysim] Indexed {} FlyWire neurons.", num_neurons);

        eprintln!("[flysim] Reading connectivity parquet: {:?}", connectivity_parquet);
        let file = File::open(connectivity_parquet)?;
        let builder = parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder::try_new(file)?;
        let arrow_reader = builder.build()?;

        // Bucket outgoing edges by presynaptic neuron index
        let mut adjacency: Vec<Vec<(u32, f64)>> = vec![Vec::new(); num_neurons];
        let mut num_edges = 0usize;

        for batch_result in arrow_reader {
            let batch = batch_result?;
            let pre_array = batch
                .column_by_name("Presynaptic_Index")
                .ok_or("Missing Presynaptic_Index")?
                .as_any()
                .downcast_ref::<Int64Array>()
                .ok_or("Invalid Presynaptic_Index type")?;

            let post_array = batch
                .column_by_name("Postsynaptic_Index")
                .ok_or("Missing Postsynaptic_Index")?
                .as_any()
                .downcast_ref::<Int64Array>()
                .ok_or("Invalid Postsynaptic_Index type")?;

            let weight_array = batch
                .column_by_name("Excitatory x Connectivity")
                .ok_or("Missing Excitatory x Connectivity")?
                .as_any()
                .downcast_ref::<Int64Array>()
                .ok_or("Invalid Excitatory x Connectivity type")?;

            for i in 0..batch.num_rows() {
                let u = pre_array.value(i) as usize;
                let v = post_array.value(i) as u32;
                let mult = weight_array.value(i) as f64;
                let w = mult * unit_weight_mv;

                if u < num_neurons {
                    adjacency[u].push((v, w));
                    num_edges += 1;
                }
            }
        }

        eprintln!("[flysim] Flattening into CSR representation ({} connections)...", num_edges);
        let mut row_offsets = Vec::with_capacity(num_neurons + 1);
        let mut col_indices = Vec::with_capacity(num_edges);
        let mut weights_mv = Vec::with_capacity(num_edges);

        let mut offset = 0;
        row_offsets.push(offset);

        for edges in &mut adjacency {
            for (v, w) in edges.drain(..) {
                col_indices.push(v);
                weights_mv.push(w);
                offset += 1;
            }
            row_offsets.push(offset);
        }

        eprintln!("[flysim] Graph CSR construction complete.");
        Ok(Self {
            num_neurons,
            num_edges,
            id_to_index,
            index_to_id,
            row_offsets,
            col_indices,
            weights_mv,
        })
    }
}
