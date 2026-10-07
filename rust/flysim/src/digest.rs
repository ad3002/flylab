//! `flysim digest`: read-only graph facts about the neurons that spiked in a finished run.
//!
//! For every neuron that spiked in any condition and trial it reports the first spike time of
//! trial 0 per condition, the shortest path length (in synapse hops) from the stimulated set and
//! the summed signed synapse count it receives directly from stimulated neurons; for
//! compare_silencing runs the same two graph facts relative to the silenced set. Readout neurons
//! get their trial-0 first spike time per condition and their strongest active presynaptic
//! partners. Every listed neuron carries its sign in the model (the sign of its outgoing synapses).
//! The LIF parameters the simulator uses are copied into the output so the interpretation cannot
//! drift from them. Nothing here simulates: the output depends only on the plan, the recorded
//! spikes and the connectome graph, so it is deterministic.

use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};

use serde::Serialize;

use crate::graph::ConnectomeGraph;
use crate::manifest::ResolvedPlan;
use crate::model::LifParameters;
use crate::output::OutputSpike;

pub const DIGEST_SCHEMA_VERSION: &str = "1.1";
/// How many active presynaptic partners are listed per readout neuron.
pub const READOUT_TOP_INPUTS: usize = 10;
/// Paths longer than this are reported as null ("not reachable within MAX_HOPS").
pub const MAX_HOPS: u8 = 4;

#[derive(Debug, Clone, Serialize, PartialEq)]
#[allow(non_snake_case)]
pub struct DigestNeuron {
    pub root_id: String,
    /// First spike time (ms) in trial 0 of condition A; null when silent in trial 0 of A.
    pub first_spike_ms_A: Option<f64>,
    /// Same for condition B; always null for single-condition runs.
    pub first_spike_ms_B: Option<f64>,
    /// 0 for a stimulated neuron, 1..=MAX_HOPS for downstream neurons, null beyond.
    pub hops_from_stimulated: Option<u8>,
    /// Summed signed synapse count from stimulated neurons onto this one (excitatory +,
    /// inhibitory -). 0 when there is no direct synapse.
    pub direct_input_from_stimulated: i64,
    /// Compare runs only (null otherwise): hops from the silenced set.
    pub hops_from_silenced: Option<u8>,
    /// Compare runs only (null otherwise): summed signed synapse count from silenced neurons.
    pub direct_input_from_silenced: Option<i64>,
    /// Sign of the neuron in the model: +1 when its outgoing synapses are excitatory, -1 when
    /// inhibitory, null when it has no outgoing synapse in the graph.
    pub model_sign: Option<i8>,
}

/// One presynaptic partner of a readout neuron that spiked in the run.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ReadoutPartner {
    pub root_id: String,
    /// Signed synapse count from this partner onto the readout (excitatory +, inhibitory -).
    pub synapses: i64,
    pub model_sign: Option<i8>,
    pub hops_from_stimulated: Option<u8>,
}

/// The direct inputs of one readout neuron.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ReadoutInputs {
    /// Every presynaptic partner in the graph, active or not.
    pub presynaptic_partners: usize,
    /// Partners that spiked in any condition or trial of the run.
    pub active_presynaptic_partners: usize,
    /// Summed signed synapse count from the active partners.
    pub net_synapses_from_active_partners: i64,
    /// The active partners with the largest |synapses| (ties by root id), at most READOUT_TOP_INPUTS.
    pub top_active_partners: Vec<ReadoutPartner>,
}

#[derive(Debug, Clone, Serialize)]
pub struct DigestGraph {
    pub schema_version: String,
    pub dataset_id: String,
    pub plan_hash: Option<String>,
    pub experiment_type: String,
    pub max_hops: u8,
    pub units: BTreeMap<String, String>,
    pub stimulated_count: usize,
    pub silenced_count: usize,
    /// Every neuron that spiked in any condition or trial, plus every readout neuron (also when
    /// silent, so its graph position is known), sorted by root id.
    pub neurons: Vec<DigestNeuron>,
    /// condition ("A", and "B" for compare runs) -> readout root id -> first spike ms (trial 0).
    pub readout_first_spike_ms: BTreeMap<String, BTreeMap<String, Option<f64>>>,
    /// readout root id -> its direct inputs from neurons active in the run.
    pub readout_inputs: BTreeMap<String, ReadoutInputs>,
    /// The LIF parameters of the simulator (model.rs), for the interpretation.
    pub lif_parameters: LifParameters,
}

/// Sign of a neuron's outgoing synapses (all share the presynaptic sign in this model).
fn model_sign(graph: &ConnectomeGraph, u: usize) -> Option<i8> {
    graph.outgoing_edges(u).find(|(_, w)| *w != 0.0).map(|(_, w)| if w > 0.0 { 1 } else { -1 })
}

/// Multi-source BFS over outgoing synapses, limited to `max_hops`. Sources get 0.
fn bfs_hops(graph: &ConnectomeGraph, sources: &[usize], max_hops: u8) -> Vec<Option<u8>> {
    let mut dist: Vec<Option<u8>> = vec![None; graph.num_neurons];
    let mut queue = VecDeque::new();
    for &s in sources {
        if dist[s].is_none() {
            dist[s] = Some(0);
            queue.push_back(s);
        }
    }
    while let Some(u) = queue.pop_front() {
        let d = dist[u].expect("queued neurons have a distance");
        if d >= max_hops {
            continue;
        }
        for (v, _) in graph.outgoing_edges(u) {
            if dist[v].is_none() {
                dist[v] = Some(d + 1);
                queue.push_back(v);
            }
        }
    }
    dist
}

/// Summed signed synapse counts from `sources` onto each target index.
fn direct_input(graph: &ConnectomeGraph, sources: &[usize], unit_weight_mv: f64) -> HashMap<usize, i64> {
    let mut acc: HashMap<usize, f64> = HashMap::new();
    for &s in sources {
        for (t, w) in graph.outgoing_edges(s) {
            *acc.entry(t).or_insert(0.0) += w;
        }
    }
    acc.into_iter()
        .map(|(t, w)| (t, (w / unit_weight_mv).round() as i64))
        .collect()
}

fn indices_of(graph: &ConnectomeGraph, ids: &[String], what: &str) -> Result<Vec<usize>, String> {
    let mut out = Vec::with_capacity(ids.len());
    let mut missing = Vec::new();
    for id in ids {
        match graph.id_to_index.get(id) {
            Some(&i) => out.push(i),
            None => missing.push(id.as_str()),
        }
    }
    if !missing.is_empty() {
        let shown: Vec<&str> = missing.iter().take(5).copied().collect();
        return Err(format!(
            "{} {} neuron id(s) are not in the connectome graph: {}{}",
            missing.len(),
            what,
            shown.join(", "),
            if missing.len() > 5 { ", ..." } else { "" }
        ));
    }
    Ok(out)
}

/// Builds the digest. `unit_weight_mv` is the weight of one synapse in the graph (0.275 mV),
/// used to report direct input in synapse counts. Spikes with an unknown condition, a condition
/// B in a single run, a root id absent from the graph, or a non-finite time are errors: the
/// digest must describe the recorded run exactly or not at all.
pub fn compute_digest(
    graph: &ConnectomeGraph,
    plan: &ResolvedPlan,
    spikes: &[OutputSpike],
    params: &LifParameters,
) -> Result<DigestGraph, String> {
    let unit_weight_mv = params.w_syn_mv;
    let is_compare = match plan.experiment_type.as_str() {
        "compare_silencing" => true,
        "single" => false,
        other => return Err(format!("unknown experiment_type {other:?} in resolved plan")),
    };
    if !(unit_weight_mv.is_finite() && unit_weight_mv > 0.0) {
        return Err(format!("unit weight must be positive (got {unit_weight_mv})"));
    }

    let mut stimulated_ids: Vec<String> = plan.activation.iter().flat_map(|a| a.neuron_ids.clone()).collect();
    stimulated_ids.sort();
    stimulated_ids.dedup();
    let stimulated = indices_of(graph, &stimulated_ids, "stimulated")?;
    let mut silenced_ids = plan.silencing_neuron_ids.clone();
    silenced_ids.sort();
    silenced_ids.dedup();
    let silenced = indices_of(graph, &silenced_ids, "silenced")?;
    let readout_idx = indices_of(graph, &plan.readout_neuron_ids, "readout")?;

    // First spike of trial 0 per condition, and the set of every neuron that spiked.
    let mut first_a: HashMap<&str, f64> = HashMap::new();
    let mut first_b: HashMap<&str, f64> = HashMap::new();
    let mut spiked: BTreeSet<&str> = BTreeSet::new();
    let mut unknown: BTreeSet<&str> = BTreeSet::new();
    for (row, s) in spikes.iter().enumerate() {
        if !s.spike_time_ms.is_finite() || s.spike_time_ms < 0.0 {
            return Err(format!("spike row {row} has an invalid time ({})", s.spike_time_ms));
        }
        let first = match s.condition.as_str() {
            "A" => &mut first_a,
            "B" if is_compare => &mut first_b,
            "B" => return Err(format!("spike row {row} has condition B in a single-condition run")),
            other => return Err(format!("spike row {row} has unknown condition {other:?}")),
        };
        if !graph.id_to_index.contains_key(&s.root_id) {
            unknown.insert(s.root_id.as_str());
            continue;
        }
        spiked.insert(s.root_id.as_str());
        if s.trial == 0 {
            // Spike times are tick * dt; round away the float noise (0.1 ms resolution).
            let t = (s.spike_time_ms * 1000.0).round() / 1000.0;
            let e = first.entry(s.root_id.as_str()).or_insert(t);
            if t < *e {
                *e = t;
            }
        }
    }
    if !unknown.is_empty() {
        let shown: Vec<&str> = unknown.iter().take(5).copied().collect();
        return Err(format!(
            "spikes reference {} root id(s) that are not in the connectome graph: {}{}",
            unknown.len(),
            shown.join(", "),
            if unknown.len() > 5 { ", ..." } else { "" }
        ));
    }

    let hops_stim = bfs_hops(graph, &stimulated, MAX_HOPS);
    let direct_stim = direct_input(graph, &stimulated, unit_weight_mv);
    let (hops_sil, direct_sil) = if is_compare {
        (Some(bfs_hops(graph, &silenced, MAX_HOPS)), Some(direct_input(graph, &silenced, unit_weight_mv)))
    } else {
        (None, None)
    };

    // Direct inputs of every readout from neurons that spiked (before readouts join `spiked`).
    let active_idx: BTreeSet<usize> = spiked.iter().map(|id| graph.id_to_index[*id]).collect();
    let readout_set: HashMap<usize, &str> =
        readout_idx.iter().zip(plan.readout_neuron_ids.iter()).map(|(&i, id)| (i, id.as_str())).collect();
    // readout index -> presynaptic index -> summed weight (mV)
    let mut incoming: HashMap<usize, HashMap<usize, f64>> = HashMap::new();
    for u in 0..graph.num_neurons {
        for (v, w) in graph.outgoing_edges(u) {
            if readout_set.contains_key(&v) {
                *incoming.entry(v).or_default().entry(u).or_insert(0.0) += w;
            }
        }
    }
    let mut readout_inputs = BTreeMap::new();
    for (&r, id) in &readout_set {
        let pre = incoming.remove(&r).unwrap_or_default();
        let mut active: Vec<ReadoutPartner> = pre
            .iter()
            .filter(|(u, _)| active_idx.contains(*u))
            .map(|(&u, &w)| ReadoutPartner {
                root_id: graph.index_to_id[u].clone(),
                synapses: (w / unit_weight_mv).round() as i64,
                model_sign: model_sign(graph, u),
                hops_from_stimulated: hops_stim[u],
            })
            .collect();
        active.sort_by(|a, b| b.synapses.abs().cmp(&a.synapses.abs()).then_with(|| a.root_id.cmp(&b.root_id)));
        let net: i64 = active.iter().map(|p| p.synapses).sum();
        let n_active = active.len();
        active.truncate(READOUT_TOP_INPUTS);
        readout_inputs.insert(
            id.to_string(),
            ReadoutInputs {
                presynaptic_partners: pre.len(),
                active_presynaptic_partners: n_active,
                net_synapses_from_active_partners: net,
                top_active_partners: active,
            },
        );
    }

    // Readout neurons are always reported, silent or not.
    for id in &plan.readout_neuron_ids {
        spiked.insert(id.as_str());
    }
    let neurons: Vec<DigestNeuron> = spiked
        .iter()
        .map(|id| {
            let idx = graph.id_to_index[*id];
            DigestNeuron {
                root_id: id.to_string(),
                first_spike_ms_A: first_a.get(id).copied(),
                first_spike_ms_B: first_b.get(id).copied(),
                hops_from_stimulated: hops_stim[idx],
                direct_input_from_stimulated: direct_stim.get(&idx).copied().unwrap_or(0),
                hops_from_silenced: hops_sil.as_ref().and_then(|h| h[idx]),
                direct_input_from_silenced: direct_sil.as_ref().map(|d| d.get(&idx).copied().unwrap_or(0)),
                model_sign: model_sign(graph, idx),
            }
        })
        .collect();

    let mut readout_first_spike_ms = BTreeMap::new();
    let mut conditions = vec![("A", &first_a)];
    if is_compare {
        conditions.push(("B", &first_b));
    }
    for (cond, first) in conditions {
        let per: BTreeMap<String, Option<f64>> = plan
            .readout_neuron_ids
            .iter()
            .map(|id| (id.clone(), first.get(id.as_str()).copied()))
            .collect();
        readout_first_spike_ms.insert(cond.to_string(), per);
    }

    let mut units = BTreeMap::new();
    units.insert("first_spike_ms".to_string(), "ms after stimulus onset, trial 0".to_string());
    units.insert(
        "direct_input".to_string(),
        "signed synapse count (excitatory +, inhibitory -) from the set directly onto the neuron".to_string(),
    );
    units.insert(
        "hops".to_string(),
        format!("shortest directed synaptic path length; 0 = member of the set; null = more than {MAX_HOPS} hops"),
    );

    Ok(DigestGraph {
        schema_version: DIGEST_SCHEMA_VERSION.to_string(),
        dataset_id: plan.dataset_id.clone(),
        plan_hash: plan.plan_hash.clone(),
        experiment_type: plan.experiment_type.clone(),
        max_hops: MAX_HOPS,
        units,
        stimulated_count: stimulated.len(),
        silenced_count: silenced.len(),
        neurons,
        readout_first_spike_ms,
        readout_inputs,
        lif_parameters: params.clone(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::ResolvedActivation;

    const W: f64 = 0.275;

    fn params() -> LifParameters {
        LifParameters::default()
    }

    /// 0 -> 1 (+3 syn), 0 -> 2 (+2 syn), 5 -> 2 (-4 syn, n5 is inhibitory), 1 -> 3 (+1), 3 -> 4 (+1),
    /// 4 -> 6 (+1), 6 -> 7 (+1). Neuron 7 is 5 hops from 0. Ids are "n0".."n7".
    fn tiny_graph() -> ConnectomeGraph {
        let edges: Vec<(usize, usize, i64)> =
            vec![(0, 1, 3), (0, 2, 2), (5, 2, -4), (1, 3, 1), (3, 4, 1), (4, 6, 1), (6, 7, 1)];
        let ids: Vec<String> = (0..8).map(|i| format!("n{i}")).collect();
        graph_from(&ids, &edges)
    }

    /// A CSR graph from (pre, post, signed synapse count) edges; every neuron's synapses share
    /// its sign, as in the model.
    fn graph_from(ids: &[String], edges: &[(usize, usize, i64)]) -> ConnectomeGraph {
        let n = ids.len();
        let mut adj: Vec<Vec<(u32, f64)>> = vec![Vec::new(); n];
        for (u, v, s) in edges {
            adj[*u].push((*v as u32, *s as f64 * W));
        }
        let mut row_offsets = vec![0usize];
        let mut col_indices = Vec::new();
        let mut weights_mv = Vec::new();
        for a in adj {
            for (v, w) in a {
                col_indices.push(v);
                weights_mv.push(w);
            }
            row_offsets.push(col_indices.len());
        }
        let index_to_id: Vec<String> = ids.to_vec();
        let id_to_index = index_to_id.iter().enumerate().map(|(i, id)| (id.clone(), i)).collect();
        ConnectomeGraph {
            num_neurons: n,
            num_edges: edges.len(),
            id_to_index,
            index_to_id,
            row_offsets,
            col_indices,
            weights_mv,
        }
    }

    fn plan(kind: &str, silenced: &[&str]) -> ResolvedPlan {
        ResolvedPlan {
            schema_version: "1.0".into(),
            plan_id: Some("plan_t".into()),
            plan_hash: Some("hash_t".into()),
            dataset_id: "flywire_630".into(),
            model_id: "shiu_lif_rust".into(),
            experiment_type: kind.into(),
            activation: vec![ResolvedActivation { neuron_ids: vec!["n0".into()], rate_hz: 100.0 }],
            silencing_neuron_ids: silenced.iter().map(|s| s.to_string()).collect(),
            readout_neuron_ids: vec!["n2".into(), "n4".into()],
            duration_ms: 100.0,
            repeats: 2,
            base_seed: 42,
            report_language: "en".into(),
            dt_ms: Some(0.1),
        }
    }

    fn spike(cond: &str, trial: usize, id: &str, t: f64) -> OutputSpike {
        OutputSpike { condition: cond.into(), trial, root_id: id.into(), spike_time_ms: t }
    }

    fn find<'a>(d: &'a DigestGraph, id: &str) -> &'a DigestNeuron {
        d.neurons.iter().find(|n| n.root_id == id).unwrap_or_else(|| panic!("{id} missing"))
    }

    #[test]
    fn compare_digest_has_hops_direct_input_and_latencies() {
        let g = tiny_graph();
        let spikes = vec![
            spike("A", 0, "n0", 1.5),
            spike("A", 0, "n0", 0.7),
            spike("A", 0, "n1", 4.0),
            spike("A", 0, "n2", 9.0),
            spike("A", 1, "n7", 3.0), // spiked only in trial 1: present, first spike null
            spike("B", 0, "n1", 5.0),
            spike("B", 0, "n4", 299.0 * 0.1), // 29.900000000000002 in f64
        ];
        let d = compute_digest(&g, &plan("compare_silencing", &["n5"]), &spikes, &params()).unwrap();

        let ids: Vec<&str> = d.neurons.iter().map(|n| n.root_id.as_str()).collect();
        assert_eq!(ids, vec!["n0", "n1", "n2", "n4", "n7"], "sorted, every spiking or readout neuron once");

        let n0 = find(&d, "n0");
        assert_eq!(n0.first_spike_ms_A, Some(0.7), "earliest spike of trial 0");
        assert_eq!(n0.first_spike_ms_B, None);
        assert_eq!(n0.hops_from_stimulated, Some(0));
        assert_eq!(n0.direct_input_from_stimulated, 0);

        let n1 = find(&d, "n1");
        assert_eq!((n1.first_spike_ms_A, n1.first_spike_ms_B), (Some(4.0), Some(5.0)));
        assert_eq!(n1.hops_from_stimulated, Some(1));
        assert_eq!(n1.direct_input_from_stimulated, 3);
        assert_eq!(n1.hops_from_silenced, None, "n5 does not reach n1");
        assert_eq!(n1.direct_input_from_silenced, Some(0));

        let n2 = find(&d, "n2");
        assert_eq!(n2.direct_input_from_stimulated, 2);
        assert_eq!(n2.hops_from_silenced, Some(1));
        assert_eq!(n2.direct_input_from_silenced, Some(-4), "inhibitory synapses count negative");
        assert_eq!(n2.model_sign, None, "no outgoing synapse: no sign");
        assert_eq!(n0.model_sign, Some(1));

        assert_eq!(find(&d, "n4").hops_from_stimulated, Some(3));
        let n7 = find(&d, "n7");
        assert_eq!(n7.hops_from_stimulated, None, "5 hops is beyond MAX_HOPS");
        assert_eq!(n7.first_spike_ms_A, None, "silent in trial 0");

        assert_eq!(d.stimulated_count, 1);
        assert_eq!(d.silenced_count, 1);
        assert_eq!(d.readout_first_spike_ms["A"]["n2"], Some(9.0));
        assert_eq!(d.readout_first_spike_ms["A"]["n4"], None);
        assert_eq!(d.readout_first_spike_ms["B"]["n4"], Some(29.9), "tick float noise is rounded");
        assert_eq!(d.readout_first_spike_ms["B"]["n2"], None);

        // n2's inputs: n0 (+2, spiked) and n5 (-4, silent). n4's only input n3 never spiked.
        let r2 = &d.readout_inputs["n2"];
        assert_eq!((r2.presynaptic_partners, r2.active_presynaptic_partners, r2.net_synapses_from_active_partners), (2, 1, 2));
        assert_eq!(r2.top_active_partners, vec![ReadoutPartner { root_id: "n0".into(), synapses: 2, model_sign: Some(1), hops_from_stimulated: Some(0) }]);
        let r4 = &d.readout_inputs["n4"];
        assert_eq!((r4.presynaptic_partners, r4.active_presynaptic_partners, r4.top_active_partners.len()), (1, 0, 0));
        assert_eq!(d.lif_parameters.t_rfc_ms, 2.2, "the simulator's parameters are copied");
        assert_eq!(d.schema_version, "1.1");
    }

    #[test]
    fn readout_inputs_are_signed_ranked_and_capped() {
        // a (+5), b (-7, inhibitory), c (+7), d (+1, silent) and 11 weak active e* (+1) onto r.
        let mut ids: Vec<String> = ["s", "r", "a", "b", "c", "d"].iter().map(|x| x.to_string()).collect();
        let mut edges = vec![(0usize, 2usize, 1i64), (0, 3, 1), (0, 4, 1), (2, 1, 5), (3, 1, -7), (4, 1, 7), (5, 1, 1)];
        for k in 0..11 {
            ids.push(format!("e{k:02}"));
            edges.push((ids.len() - 1, 1, 1));
        }
        let g = graph_from(&ids, &edges);
        let mut p = plan("single", &[]);
        p.activation[0].neuron_ids = vec!["s".into()];
        p.readout_neuron_ids = vec!["r".into()];
        let mut spikes = vec![spike("A", 0, "s", 1.0), spike("A", 0, "a", 3.0), spike("A", 0, "b", 3.0), spike("A", 1, "c", 4.0)];
        for k in 0..11 {
            spikes.push(spike("A", 0, &format!("e{k:02}"), 5.0));
        }
        let d = compute_digest(&g, &p, &spikes, &params()).unwrap();
        let r = &d.readout_inputs["r"];
        assert_eq!(r.presynaptic_partners, 15);
        assert_eq!(r.active_presynaptic_partners, 14, "d never spiked");
        assert_eq!(r.net_synapses_from_active_partners, 5 - 7 + 7 + 11);
        assert_eq!(r.top_active_partners.len(), READOUT_TOP_INPUTS);
        let order: Vec<(&str, i64, Option<i8>)> =
            r.top_active_partners.iter().map(|x| (x.root_id.as_str(), x.synapses, x.model_sign)).collect();
        assert_eq!(&order[..4], &[("b", -7, Some(-1)), ("c", 7, Some(1)), ("a", 5, Some(1)), ("e00", 1, Some(1))]);
        assert_eq!(r.top_active_partners[0].hops_from_stimulated, Some(1));
        assert_eq!(find(&d, "b").model_sign, Some(-1));
    }

    #[test]
    fn single_digest_has_null_silenced_fields_and_is_deterministic() {
        let g = tiny_graph();
        let spikes = vec![spike("A", 0, "n3", 6.0), spike("A", 0, "n1", 2.0)];
        let p = plan("single", &[]);
        let d1 = compute_digest(&g, &p, &spikes, &params()).unwrap();
        let mut reversed = spikes.clone();
        reversed.reverse();
        let d2 = compute_digest(&g, &p, &reversed, &params()).unwrap();
        assert_eq!(
            serde_json::to_string(&d1).unwrap(),
            serde_json::to_string(&d2).unwrap(),
            "spike order must not change the digest"
        );
        let ids: Vec<&str> = d1.neurons.iter().map(|n| n.root_id.as_str()).collect();
        assert_eq!(ids, vec!["n1", "n2", "n3", "n4"], "silent readouts n2 and n4 are still reported");
        let n2 = find(&d1, "n2");
        assert_eq!((n2.first_spike_ms_A, n2.hops_from_stimulated, n2.direct_input_from_stimulated), (None, Some(1), 2));
        let n3 = find(&d1, "n3");
        assert_eq!(n3.hops_from_stimulated, Some(2));
        assert_eq!(n3.hops_from_silenced, None);
        assert_eq!(n3.direct_input_from_silenced, None);
        assert!(!d1.readout_first_spike_ms.contains_key("B"));
        let json = serde_json::to_value(&d1).unwrap();
        assert!(json["neurons"][0]["first_spike_ms_B"].is_null());
        assert!(json["neurons"][0]["direct_input_from_silenced"].is_null());
        assert_eq!(json["max_hops"], 4);
    }

    #[test]
    fn invalid_spikes_are_errors_not_skips() {
        let g = tiny_graph();
        let single = plan("single", &[]);
        let err = compute_digest(&g, &single, &[spike("B", 0, "n1", 1.0)], &params()).unwrap_err();
        assert!(err.contains("condition B in a single-condition run"), "{err}");
        let err = compute_digest(&g, &single, &[spike("C", 0, "n1", 1.0)], &params()).unwrap_err();
        assert!(err.contains("unknown condition"), "{err}");
        let err = compute_digest(&g, &single, &[spike("A", 0, "n99", 1.0)], &params()).unwrap_err();
        assert!(err.contains("not in the connectome graph: n99"), "{err}");
        let err = compute_digest(&g, &single, &[spike("A", 0, "n1", f64::NAN)], &params()).unwrap_err();
        assert!(err.contains("invalid time"), "{err}");
        let mut bad = plan("single", &[]);
        bad.readout_neuron_ids = vec!["nX".into()];
        let err = compute_digest(&g, &bad, &[], &params()).unwrap_err();
        assert!(err.contains("readout neuron id(s) are not in the connectome graph: nX"), "{err}");
    }
}
