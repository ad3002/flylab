use serde::{Deserialize, Serialize};

/// Leaky Integrate-and-Fire parameters matching Brian2 upstream model.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LifParameters {
    pub v_0_mv: f64,
    pub v_rst_mv: f64,
    pub v_th_mv: f64,
    pub t_mbr_ms: f64,
    pub tau_ms: f64,
    pub t_rfc_ms: f64,
    pub t_dly_ms: f64,
    pub w_syn_mv: f64,
    pub f_poi: f64,
    pub dt_ms: f64,
}

impl Default for LifParameters {
    fn default() -> Self {
        Self {
            v_0_mv: -52.0,
            v_rst_mv: -52.0,
            v_th_mv: -45.0,
            t_mbr_ms: 20.0,
            tau_ms: 5.0,
            t_rfc_ms: 2.2,
            t_dly_ms: 1.8,
            w_syn_mv: 0.275,
            f_poi: 250.0,
            dt_ms: 0.1,
        }
    }
}

/// Precomputed coefficients for analytical integration step.
#[derive(Debug, Clone)]
pub struct AnalyticalCoefficients {
    pub c_g: f64,
    pub c_v: f64,
    pub c_vg: f64,
    pub delay_ticks: usize,
    pub default_rfc_ticks: i64,
    pub stimulus_step_mv: f64,
}

impl AnalyticalCoefficients {
    pub fn new(p: &LifParameters) -> Self {
        let dt = p.dt_ms;
        let c_g = (-dt / p.tau_ms).exp();
        let c_v = (-dt / p.t_mbr_ms).exp();
        let c_vg = (p.tau_ms / (p.t_mbr_ms - p.tau_ms)) * (c_v - c_g);
        let delay_ticks = (p.t_dly_ms / dt).round() as usize;
        let default_rfc_ticks = (p.t_rfc_ms / dt).round() as i64;
        let stimulus_step_mv = p.w_syn_mv * p.f_poi;

        Self {
            c_g,
            c_v,
            c_vg,
            delay_ticks,
            default_rfc_ticks,
            stimulus_step_mv,
        }
    }
}

/// Recorded spike event.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SpikeRecord {
    pub neuron_idx: usize,
    pub tick: usize,
    pub time_ms: f64,
}

/// Network state for simulation.
pub struct NetworkState {
    pub v: Vec<f64>,
    pub g: Vec<f64>,
    pub last_spike_tick: Vec<i64>,
    pub rfc_ticks: Vec<i64>,
    pub ring_buffer: Vec<Vec<f64>>,
    pub ring_len: usize,
}

impl NetworkState {
    pub fn new(num_neurons: usize, params: &LifParameters, coeffs: &AnalyticalCoefficients) -> Self {
        let ring_len = coeffs.delay_ticks + 1; // 18 + 1 = 19
        let ring_buffer = vec![vec![0.0; num_neurons]; ring_len];

        Self {
            v: vec![params.v_0_mv; num_neurons],
            g: vec![0.0; num_neurons],
            last_spike_tick: vec![-1_000_000; num_neurons],
            rfc_ticks: vec![coeffs.default_rfc_ticks; num_neurons],
            ring_buffer,
            ring_len,
        }
    }

    /// Reset state completely for a fresh trial.
    pub fn reset(&mut self, params: &LifParameters, coeffs: &AnalyticalCoefficients) {
        for v_i in &mut self.v {
            *v_i = params.v_0_mv;
        }
        for g_i in &mut self.g {
            *g_i = 0.0;
        }
        for s_i in &mut self.last_spike_tick {
            *s_i = -1_000_000;
        }
        for r_i in &mut self.rfc_ticks {
            *r_i = coeffs.default_rfc_ticks;
        }
        for slot in &mut self.ring_buffer {
            for val in slot.iter_mut() {
                *val = 0.0;
            }
        }
    }

    /// Set refractory period to 0 for Poisson stimulated target neurons.
    pub fn set_poisson_targets(&mut self, target_indices: &[usize]) {
        for &idx in target_indices {
            if idx < self.rfc_ticks.len() {
                self.rfc_ticks[idx] = 0;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_analytical_coefficients() {
        let p = LifParameters::default();
        let c = AnalyticalCoefficients::new(&p);
        assert_eq!(c.delay_ticks, 18);
        assert_eq!(c.default_rfc_ticks, 22);

        // Check against Brian2 known values
        let exp_cg = (-0.1f64 / 5.0).exp();
        assert!((c.c_g - exp_cg).abs() < 1e-15);

        let exp_cv = (-0.1f64 / 20.0).exp();
        assert!((c.c_v - exp_cv).abs() < 1e-15);
    }
}
