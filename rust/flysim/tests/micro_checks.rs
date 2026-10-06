use std::fs::File;
use std::io::{BufRead, BufReader};
use std::path::Path;

use flysim::model::{AnalyticalCoefficients, LifParameters};

#[test]
fn test_micro_decay_analytical_match() {
    let trace_path = Path::new("../../reference/traces/micro/micro_decay.csv");
    if !trace_path.exists() {
        eprintln!("Warning: reference micro_decay.csv not found at {:?}, skipping", trace_path);
        return;
    }

    let file = File::open(trace_path).expect("open micro_decay.csv");
    let reader = BufReader::new(file);

    let params = LifParameters::default();
    let coeffs = AnalyticalCoefficients::new(&params);

    let mut v = -48.0;
    let mut g = 2.0;

    for (line_idx, line) in reader.lines().enumerate() {
        let line = line.expect("read line");
        if line_idx == 0 {
            continue; // header: step,t_ms,v_mv,g_mv,not_refractory
        }

        let parts: Vec<&str> = line.split(',').collect();
        let expected_v: f64 = parts[2].parse().expect("parse v");
        let expected_g: f64 = parts[3].parse().expect("parse g");

        // Verify with absolute tolerance 1e-8 mV as required by spec
        let diff_v = (v - expected_v).abs();
        let diff_g = (g - expected_g).abs();

        assert!(
            diff_v < 1e-8,
            "Step {}: v diff {} exceeds tolerance 1e-8 (rust: {}, brian: {})",
            line_idx - 1,
            diff_v,
            v,
            expected_v
        );
        assert!(
            diff_g < 1e-8,
            "Step {}: g diff {} exceeds tolerance 1e-8 (rust: {}, brian: {})",
            line_idx - 1,
            diff_g,
            g,
            expected_g
        );

        // Analytical update for next step
        let g_prev = g;
        g *= coeffs.c_g;
        v = params.v_0_mv + (v - params.v_0_mv) * coeffs.c_v + g_prev * coeffs.c_vg;
    }
}

#[test]
fn test_spontaneous_quiescence_zero_input() {
    // Model with 0 stimulus should have 0 spontaneous spikes
    let params = LifParameters::default();
    let coeffs = AnalyticalCoefficients::new(&params);

    let mut v = params.v_0_mv;
    let mut g = 0.0;

    for _ in 0..10_000 {
        let g_prev = g;
        g *= coeffs.c_g;
        v = params.v_0_mv + (v - params.v_0_mv) * coeffs.c_v + g_prev * coeffs.c_vg;
        assert!(v <= params.v_th_mv, "Spontaneous threshold breach");
    }
}
