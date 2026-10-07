//! Input events must never be dropped silently when they are indexed for simulation.
//!
//! Regression: `index_events_by_tick` skipped events whose root id was not in the graph, so
//! `flysim replay` against another cache or connectome version simulated a smaller stimulus
//! and still reported `status: replayed`.

use std::collections::HashMap;

use flysim::input::{index_events_by_tick, InputEvent};

fn ev(trial: usize, root_id: &str, timestep: usize) -> InputEvent {
    InputEvent { trial, root_id: root_id.to_string(), timestep, multiplicity: 1 }
}

#[test]
fn known_ids_are_indexed_per_trial() {
    let id_to_index: HashMap<String, usize> =
        [("n1".to_string(), 0usize), ("n2".to_string(), 1usize)].into_iter().collect();
    let events = vec![ev(0, "n1", 3), ev(0, "n2", 3), ev(1, "n1", 5)];
    let map = index_events_by_tick(&events, &id_to_index, 0).expect("all ids are known");
    assert_eq!(map.len(), 1);
    let mut at3 = map[&3].clone();
    at3.sort();
    assert_eq!(at3, vec![(0, 1), (1, 1)]);
    let map1 = index_events_by_tick(&events, &id_to_index, 1).expect("all ids are known");
    assert_eq!(map1[&5], vec![(0, 1)]);
}

#[test]
fn unknown_ids_are_an_error_naming_them() {
    let id_to_index: HashMap<String, usize> = [("n1".to_string(), 0usize)].into_iter().collect();
    let events = vec![ev(0, "n1", 1), ev(0, "gone_42", 2), ev(0, "gone_42", 4), ev(1, "other", 1)];
    let err = index_events_by_tick(&events, &id_to_index, 0).expect_err("unknown id must fail");
    assert!(err.contains("2 input event(s) of trial 0"), "{err}");
    assert!(err.contains("1 root id(s)"), "{err}");
    assert!(err.contains("gone_42"), "{err}");
    assert!(!err.contains("other"), "trial 1 ids must not be reported for trial 0: {err}");
}
