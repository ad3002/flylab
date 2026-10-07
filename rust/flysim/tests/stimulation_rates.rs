//! Every activation set must be driven at its own rate_hz.
//!
//! Regression: flysim used `plan.activation.first().rate_hz` for every stimulated neuron, so
//! "sugar 40 Hz + bitter 160 Hz" silently ran bitter at 40 Hz while the report and UI said 160 Hz.

use std::collections::HashMap;

use flysim::input::{
    generate_deterministic_events, generate_deterministic_events_with_rates, stimulation_rates,
};
use flysim::manifest::ResolvedActivation;

fn ids(prefix: &str, n: usize) -> Vec<String> {
    (0..n).map(|i| format!("{prefix}{i:03}")).collect()
}

#[test]
fn each_activation_set_keeps_its_own_rate() {
    let activation = vec![
        ResolvedActivation { neuron_ids: ids("sugar_", 20), rate_hz: 40.0 },
        ResolvedActivation { neuron_ids: ids("bitter_", 20), rate_hz: 160.0 },
    ];
    let rates = stimulation_rates(&activation).expect("disjoint sets must resolve");
    assert_eq!(rates.len(), 40);
    let by_id: HashMap<_, _> = rates.iter().cloned().collect();
    assert_eq!(by_id["sugar_000"], 40.0);
    assert_eq!(by_id["bitter_019"], 160.0);

    // 1000 ms at dt 0.1 ms = 10 000 Bernoulli draws per neuron.
    let events = generate_deterministic_events_with_rates(&rates, 1000.0, 7, 0.1, 0);
    let mut per_set: HashMap<&str, usize> = HashMap::new();
    for e in &events {
        let set = if e.root_id.starts_with("sugar_") { "sugar" } else { "bitter" };
        *per_set.entry(set).or_default() += 1;
    }
    let sugar_hz = per_set["sugar"] as f64 / 20.0;
    let bitter_hz = per_set["bitter"] as f64 / 20.0;
    assert!((sugar_hz - 40.0).abs() < 6.0, "sugar realised {sugar_hz} Hz, expected ~40");
    assert!((bitter_hz - 160.0).abs() < 12.0, "bitter realised {bitter_hz} Hz, expected ~160");
}

#[test]
fn single_set_is_bit_identical_to_the_brian2_parity_generator() {
    let stim = ids("n", 21);
    let activation = vec![ResolvedActivation { neuron_ids: stim.clone(), rate_hz: 50.0 }];
    let rates = stimulation_rates(&activation).unwrap();
    let new = generate_deterministic_events_with_rates(&rates, 100.0, 42, 0.1, 1);
    let old = generate_deterministic_events(&stim, 100.0, 50.0, 42, 0.1, 1);
    assert!(!old.is_empty());
    assert_eq!(new.len(), old.len());
    for (a, b) in new.iter().zip(old.iter()) {
        assert_eq!((a.trial, &a.root_id, a.timestep, a.multiplicity), (b.trial, &b.root_id, b.timestep, b.multiplicity));
    }
}

#[test]
fn neuron_in_two_sets_is_an_error_naming_it() {
    let activation = vec![
        ResolvedActivation { neuron_ids: vec!["a".into(), "shared".into()], rate_hz: 40.0 },
        ResolvedActivation { neuron_ids: vec!["shared".into(), "b".into()], rate_hz: 160.0 },
    ];
    let err = stimulation_rates(&activation).expect_err("overlap must be rejected");
    assert!(err.contains("shared"), "error must name the neuron: {err}");
    assert!(err.contains("activation[0]") && err.contains("activation[1]"), "error must name both sets: {err}");
}

#[test]
fn empty_or_invalid_activation_is_an_error() {
    assert!(stimulation_rates(&[]).is_err());
    let bad = vec![ResolvedActivation { neuron_ids: vec!["x".into()], rate_hz: f64::NAN }];
    assert!(stimulation_rates(&bad).unwrap_err().contains("rate_hz"));
}
