use flysim::output::{load_spikes_parquet, save_spikes_parquet, OutputSpike};

#[test]
fn spikes_parquet_roundtrip_preserves_every_row() {
    let dir = tempfile::tempdir().expect("tempdir");
    let path = dir.path().join("spikes.parquet");
    let spikes = vec![
        OutputSpike { condition: "A".into(), trial: 0, root_id: "720575940645521262".into(), spike_time_ms: 12.3 },
        OutputSpike { condition: "B".into(), trial: 2, root_id: "720575940660219265".into(), spike_time_ms: 0.1 },
    ];
    save_spikes_parquet(&spikes, &path).expect("save");
    let back = load_spikes_parquet(&path).expect("load");
    assert_eq!(back.len(), 2);
    assert_eq!(back[0].condition, "A");
    assert_eq!(back[0].root_id, "720575940645521262");
    assert_eq!(back[0].spike_time_ms, 12.3);
    assert_eq!(back[1].trial, 2);
    assert_eq!(back[1].condition, "B");
}

#[test]
fn spikes_parquet_missing_file_is_an_error() {
    let dir = tempfile::tempdir().expect("tempdir");
    let err = load_spikes_parquet(&dir.path().join("nope.parquet")).unwrap_err();
    assert!(err.to_string().to_lowercase().contains("no such file"), "{err}");
}
