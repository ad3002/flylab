# FlyLab v3 contract: spikes → text (AI hypotheses)

Goal: after a run, the expert can ask Claude what the activity might mean. The output is a set of
**AI hypotheses for an expert to evaluate**, never presented as established fact. Every hypothesis
must be grounded in numbers the system actually computed, and should come with a runnable
follow-up experiment that would test it.

The pipeline is split so that everything factual is deterministic and testable, and the LLM only
interprets:

```
run artifacts ──► deterministic digest (Go + flysim digest) ──► claude -p ──► validated hypotheses
                    facts, numbers, annotations, coverage          interpretation      + test plans
```

## 1. Neuron annotations (data)

- Source: `flyconnectome/flywire_annotations`, `supplemental_files/Supplemental_file1_neuron_annotations.tsv`
  (Schlegel et al., Nature 2024). Pin a specific commit SHA and the file's SHA-256; record both, the
  URL and the citation in `data/dataset_manifest.json` under `files.annotations` (optional entry).
- Its `root_id` is materialization 783. **83.4 %** of the 127,400 v630 neurons have the same root id
  (measured: 106,214 / 127,400); the rest were edited between versions and stay unannotated.
  No silent pretending: coverage is reported everywhere it matters (see §3).
- `scripts/setup_data.sh` downloads the TSV (if missing), verifies the SHA-256, and derives
  `data/annotations_630.tsv` with columns `root_id, flow, super_class, cell_class, cell_sub_class,
  cell_type, hemibrain_type, top_nt, top_nt_conf, side, nerve` for v630 ids only. Raw and derived
  files are git-ignored like the other data files.
- The server loads `annotations_630.tsv` at startup. Missing file = expected absence: the server
  runs, `/capabilities` reports `annotations_ready:false`, and every digest carries coverage 0 with a
  visible warning. Present but malformed = startup error (no silent skip).
- Registry groups already annotate cleanly (e.g. sugar_grn → sensory/gustatory LB3*/LB4b, mn9 →
  motor CB0701) — use this in a test.

## 2. `flysim digest` (Rust)

New subcommand, read-only over existing artifacts:

```
flysim digest --resolved-plan resolved_plan.json --spikes spikes.parquet \
              --manifest manifest.json --cache-dir data/cache --output digest_graph.json
```

For every neuron that spiked in any condition it outputs:

- `first_spike_ms_A`, `first_spike_ms_B` (null if silent in that condition; trial 0),
- `hops_from_stimulated`: shortest path length (1..4, else null) from the stimulated set over the
  directed synapse graph,
- `direct_input_from_stimulated`: summed signed weight (excitatory +, inhibitory −) of synapses
  coming directly from stimulated neurons,
- `hops_from_silenced` and `direct_input_from_silenced` (compare runs only).

Plus per condition: `first_spike_ms` of each readout neuron. Deterministic; unit test on a tiny
graph; must not change any existing artifact or the 267/266 reference.

The worker runs `flysim digest` lazily the first time an interpretation is requested (or right
after a successful run — implementer's choice, but it must not slow the run's success status) and
stores `digest_graph.json` in the job's artifact dir.

## 3. Deterministic digest (Go, `internal/interpret`)

`BuildDigest(job) → Digest` (JSON, also returned to the client for transparency). Contents:

- experiment: type, duration, repeats, seed; each stimulated group with rate, neuron count and its
  annotation summary; silenced neurons with annotations; readout neurons with annotations.
- totals per condition: spikes, active neurons; for compare runs: Δ and relative change.
- activity by `super_class`, by `cell_class` and by `top_nt`: active-neuron count, total spikes,
  mean rate, per condition, plus Δ for compare runs (top 15 classes by spikes).
- top 30 neurons by rate (A) with all annotation fields, rate A/B, Δ, first-spike latency,
  hops/direct input from stimulated.
- for compare runs: top 20 neurons by |Δ rate| (both directions), same fields.
- readout neurons: rates, Δ, latency, plus deterministic **behavioural proxies** from
  `registry/readout_proxies.json` (new; e.g. MN9 → "proboscis extension / feeding initiation motor
  output", reference Shiu et al. 2024; each proxy has `neuron_ids`, `behaviour_en`, `behaviour_ru`,
  `evidence`, `reference`). Proxies are facts about the literature, not about this run.
- `coverage`: share of active neurons annotated (overall and per condition), share of total spikes
  from annotated neurons, list of unannotated top neurons. If coverage < 70 % the digest carries a
  warning string.
- model facts the LLM must respect: LIF point neurons, weights from synapse counts × sign of
  predicted neurotransmitter, no neuromodulation, no plasticity, no body/sensory feedback, Poisson
  input on stimulated neurons, silencing removes **outgoing** synapses only (upstream audit).

Table-driven tests with a fixture job directory (small rates/comparison/digest_graph files).

## 4. API

| Method + path | Auth | Behaviour |
| --- | --- | --- |
| `POST /api/v1/jobs/{job_id}/interpretation` body `{language?: "ru"\|"en", regenerate?: bool}` | owner | job must be `succeeded` (else 409 `JOB_NOT_COMPLETED`). Returns the cached interpretation unless `regenerate`. Otherwise builds digest, calls Claude, validates, stores, returns 200. Uses the planner's concurrency semaphore and per-user/IP/global rate limits (separate counters, `INTERPRET_RATE_LIMIT_PER_HOUR`, default 20). |
| `GET /api/v1/jobs/{job_id}/interpretation` | owner | latest stored interpretation or 404 `INTERPRETATION_NOT_FOUND`. |
| `GET /api/v1/jobs/{job_id}/digest` | owner | the deterministic digest only (no LLM call). |

Storage: table `interpretations(job_id TEXT PRIMARY KEY, language TEXT, model TEXT,
created_at TIMESTAMP, cost_usd REAL, duration_ms INTEGER, digest_json TEXT, result_json TEXT)`,
created idempotently at startup. `HistoryJob` gains `has_interpretation: bool`.

Claude call: same CLI flags as the planner (`--tools ""`, stdin prompt, `--json-schema`,
`--output-format json`, no session persistence), model `CLAUDE_INTERPRET_MODEL`
(default `claude-opus-5-5`), timeout `CLAUDE_INTERPRET_TIMEOUT_SECONDS` (default 180). The user
message is the digest JSON plus the original prompt/title of the run.

System prompt requirements (tested by inspecting the generated prompt):

- Role: hypothesis generator for a computational neuroscientist; output is a set of hypotheses for
  expert review, not conclusions.
- Separate **observations** (restating digest numbers, no interpretation) from **hypotheses**.
- Every hypothesis cites evidence as digest references (class names, cell types, neuron ids, numbers
  exactly as in the digest). Never invent cell types, neurotransmitters, or numbers not in the digest.
- State model limits when relevant (list from §3); never claim whole-animal behaviour as an outcome —
  only as a hypothesis linked to a proxy, with the proxy's evidence.
- Calibrated confidence: `low|medium|high` with a one-line reason; unannotated neurons and low
  coverage lower confidence.
- For each hypothesis propose a discriminating follow-up experiment expressible in this platform
  (activation/silencing/readout of registry groups or explicit neuron ids from the digest, rates
  0–200 Hz, 10–1000 ms, repeats 1–3).
- Answer in the requested language.

Output schema:

```json
{
  "headline": "one sentence",
  "observations": [{"text": "...", "evidence": ["..."]}],
  "hypotheses": [{
    "title": "...", "statement": "...",
    "confidence": "low|medium|high", "confidence_reason": "...",
    "evidence": ["..."], "caveats": ["..."],
    "test": {"description": "...", "expected_if_true": "...", "plan": { planner plan shape }}
  }],
  "limitations": ["..."],
  "suggested_reading": ["citation strings already present in the digest/proxies only"]
}
```

Post-processing (deterministic): each `test.plan` is validated with the existing validator and
stored with `plan_id` or `plan_error` (never dropped). Evidence strings that name neuron ids not in
the digest are flagged in `evidence_warnings` (visible in UI). Failure to run or parse Claude →
502 `LLM_ERROR` with reason, nothing cached.

Response: `{interpretation: {...schema..., tests with plan_id/plan_error}, digest, meta: {model,
cost_usd, duration_ms, created_at, language, cached: bool}, disclaimer, evidence_warnings}` where
`disclaimer` is a fixed server-side text (ru/en): "AI-generated hypotheses about a computational
model. They are not established biological findings and must be evaluated by an expert."

## 5. UI (job detail `#/job/<id>` and run results)

- Panel "AI hypotheses" / "Гипотезы ИИ" with a permanent, unmissable label: AI-generated
  assumptions for expert review. Button "Generate hypotheses" (shows model name), living loading
  state, then: headline; observations (styled as data, with evidence chips); hypothesis cards with
  confidence badge + reason, statement, evidence chips (neuron ids/cell types clickable to filter the
  raster/table if feasible), caveats, and the proposed test with "Run this test" (uses `plan_id`;
  shows `plan_error` inline if invalid) which launches a new job titled from the hypothesis.
- Coverage warning banner when the digest has one; `evidence_warnings` shown inline.
- "What the AI saw" collapsible: the digest rendered as readable tables (classes, top neurons,
  proxies, coverage) — transparency for the expert.
- Regenerate (with language toggle ru/en). Cost and model in the footer of the panel.
- Library cards show a small "AI" marker when `has_interpretation`.
- After a successful run in the composer flow, a CTA "Ask AI what this might mean".
- Landing: add a short step/section on hypotheses with the same disclaimer wording.

## 6. Existing (pre-v3) runs

Every already-finished successful run must be interpretable without re-running it: its
`digest_graph.json` does not exist yet, so the first interpretation request runs `flysim digest`
on the stored `spikes.parquet` + `resolved_plan.json` and caches the file. A missing
`spikes.parquet` for a succeeded job is a visible error (`digest_error`), not a silent skip.

UI: any succeeded run with `has_interpretation:false` shows an "Interpret" / "Интерпретировать"
button — on its Library card (small action, does not open the run) and prominently in the job
detail panel. Runs that already have one show "View hypotheses" and a Regenerate action instead.

## 7. Backend implementation notes (implemented; extend, do not contradict, sections 1-6)

- `digest_graph.json` is generated lazily on the first `GET .../digest` or `POST .../interpretation`
  (not after the run, so the run's success status is never delayed) and cached in the run's artifact
  directory; no existing artifact is modified. It lists every neuron that spiked **plus every readout
  neuron** (silent readouts keep their graph position). `hops_*` is 0 for members of the set itself.
  `direct_input_*` is a signed **synapse count** (not mV). First-spike times are rounded to 0.001 ms.
  A run whose `manifest.json` names other connectome files than the server's is a `digest_error`.
- `digest_graph.json` (schema `1.1`) also carries per neuron `model_sign` (+1/-1 = sign of its outgoing
  synapses in the simulation, null without outgoing synapses), per readout `readout_inputs` (all and
  active presynaptic partner counts, net signed synapses from active partners, the 10 active partners
  with the largest |synapses|) and the simulator's `lif_parameters` (model.rs).
- A cached `digest_graph.json` that cannot be used is derived data and is recomputed once from
  `spikes.parquet` on any `GET .../digest` or `POST .../interpretation` (no `regenerate` needed, so an
  old run is never stuck): silently when it was written by an older flysim (schema_version), with a
  visible entry in the digest's `warnings` when it was unreadable, truncated or did not match the run.
  Only when the recompute fails too is it a `DIGEST_ERROR`.
- Digest failures (missing `spikes.parquet`, failing `flysim digest`, corrupt `rates.csv`, ...) answer
  **500** with `error.code = "DIGEST_ERROR"`, `error.details.digest_error` and the same text in a
  top-level `digest_error` field. The single digest slot staying taken for 20 s is **503**
  `DIGEST_BUSY` with `Retry-After` (temporary, not a digest error).
- `POST .../interpretation`: `language` defaults to `"en"`; anything but `en`/`ru` is 422
  `INVALID_LANGUAGE`; an unknown body field is 422 `INVALID_REQUEST_BODY`. Without `regenerate` a stored
  interpretation is returned as is (`meta.cached: true`, `meta.language` = the stored language, which may
  differ from the requested one). A stored row that cannot be parsed is 500 `INTERPRETATION_CORRUPT`; the UI
  then offers "Replace with new hypotheses", which sends `regenerate: true`. Rate limits apply only to new
  Claude calls: the limits are checked on admission but a unit is recorded only once the digest is built
  and a Claude slot is held, so `DIGEST_ERROR`, `DIGEST_BUSY` and `LLM_BUSY` cost no quota. 429 `RATE_LIMITED` with
  `details.scope` = `user` (`INTERPRET_RATE_LIMIT_PER_HOUR`, 20), `ip`
  (`INTERPRET_RATE_LIMIT_PER_IP_PER_HOUR`, 40), `global` (`INTERPRET_GLOBAL_LIMIT_PER_HOUR`, 100) or
  `user_in_flight` (one interpretation at a time per account). No free Claude slot within 30 s is 503
  `LLM_BUSY`. A missing `claude` binary is 502 `LLM_ERROR` (no heuristic fallback for interpretations).
  A real call with `claude-opus-5-5` takes about 2 minutes (~$0.5 on a 40 KB digest).
- Once admitted, the work runs on a context detached from the client connection (bounded by its own
  timeouts), so a reload or a closed tab does not kill a paid Claude call: the result is stored and the
  next `GET` finds it. Worst case of one POST = digest slot wait 20 s + flysim digest 60 s + Claude slot
  wait 30 s + `CLAUDE_INTERPRET_TIMEOUT_SECONDS` (180) = 290 s, below nginx `proxy_read_timeout 300s`
  (tested; raising the Claude timeout needs a larger proxy timeout).
- Digest top-level keys: `schema_version, job_id, title, experiment{type, dataset_id, model_id,
  duration_ms, repeats, base_seed, stimulated[], silenced|null, readout}, totals{A, B|null, delta_spikes,
  relative_change_spikes, delta_active_neurons, relative_change_active_neurons},
  activity_by_super_class|activity_by_cell_class|activity_by_top_nt|activity_by_cell_type {field,
  classes_total, note, rows[{class, A, B|null, delta_spikes, delta_active_neurons, delta_mean_rate_hz}]},
  top_neurons_by_rate[] (non-stimulated neurons only: a stimulated neuron's rate is set by its Poisson
  input), top_neurons_by_delta[]|null, stimulated_neurons[] (every stimulated neuron, up to 100),
  silenced_neurons[]|null (up to 50), readouts[], readout_inputs[{readout_id, root_id, synapses,
  model_sign, hops_from_stimulated, roles, annotation, rate_A_hz, rate_B_hz, delta_hz, spike_count_A,
  spike_count_B}], behavioural_proxies[], coverage{annotations_ready, annotations_source, active_neurons,
  active_annotated, active_annotated_share, spike_share_annotated, per_condition{A,B},
  unannotated_top_neurons[], warning|null, per_field{super_class|cell_class|cell_sub_class|cell_type|top_nt:
  {active_with_value, share}}, field_warnings[]}, graph_units, model_parameters{LIF parameters,
  threshold_above_rest_mv, poisson_input_step_mv, max_rate_hz_bound, note}, model_facts[], references[],
  warnings[]` (schema_version `1.1`). A neuron row: `root_id, roles[stimulated|silenced|readout], groups[],
  annotation{flow, super_class, cell_class, cell_sub_class, cell_type, hemibrain_type, top_nt, top_nt_conf,
  side, nerve}|null, model_sign, rate_A_hz, rate_B_hz, delta_hz, spike_count_A, spike_count_B,
  first_spike_ms_A, first_spike_ms_B, hops_from_stimulated, direct_input_from_stimulated,
  hops_from_silenced, direct_input_from_silenced` (B and silenced fields are null for single runs); readouts
  add `proxy_ids, presynaptic_partners, active_presynaptic_partners, net_synapses_from_active_partners`.
  Class `"(unannotated)"` = neuron without annotation; `"(not annotated in this field)"` = annotated neuron
  with an empty field. `delta_mean_rate_hz` is over the class's neurons active in A or B (0 Hz where
  silent), so its sign matches `delta_spikes`. A field filled for < 70 % of active neurons gets a
  `field_warnings` entry (also in `warnings`); in practice `cell_class` is mostly empty for central neurons
  while `cell_type` is filled. Neuron sets in `experiment` carry `group_id` when they equal a registry group,
  `neuron_ids`, and an `annotation_summary` (counts by super class, cell class, sub class, cell type, top_nt,
  side, plus `ids_by_cell_sub_class` and `ids_by_cell_type`).
- Post-processing adds deterministic checks to every hypothesis (on generation and on every read of a
  stored one): `test.warnings[]` (a `single` plan whose `expected_if_true` names B-only fields such as
  `delta_hz`, `rate_B_hz` or "condition B"; a description asking for `compare_silencing` with a `single`
  plan; `expected_if_true` quoting this run's numbers, which FlyLab never compares across runs) and
  `calibration_warning` (medium/high confidence whose reason says the claim was not tested in this run, or
  whose evidence cites only proxies, literature or setup). The UI shows both next to the test and the
  confidence badge.
- `HistoryJob` also carries `interpretation_language` (`"ru"`/`"en"`, null without an interpretation); the
  Library card and the run page label "View hypotheses" in that language.
- `evidence_warnings[]`: `{location, kind: "unknown_neuron_id" | "unknown_reference", neuron_id?, reference?,
  text, message}`. Checked: observations' and hypotheses' evidence, hypothesis statements, neuron ids in
  test plans, and `suggested_reading` against the digest's `references`.
- `GET /capabilities` adds `annotations_ready`, `annotations_count`, `interpret_model`, `interpret_ready`.
- Annotation source: `flyconnectome/flywire_annotations` commit
  `a83b2776d60d5764cef36b927f5f9679c16c47a2`, file SHA-256
  `b214970b55d2fbe0853bba536fdcb9e28730f4eb7ab06f600491df795da683cd`; 106,214 of 127,400 v630 neurons
  (83.4 %) annotated. Registry facts: all 20 annotated `sugar_grn` neurons are sensory/gustatory LB3b/c/d
  or LB4b (cell_sub_class includes 5 `high_salt/heavy_metal` LB3d and 4 `putative_attractive` LB4b), one is
  unannotated; `mn9` 720575940660219265 is motor CB0701 (annotation side `right`), 720575940645521262 is
  unannotated (edited after v630). Side convention: FlyWire found the FAFB images left/right inverted and
  corrected all side labels to the fly's own side (soma position; codex.flywire.ai FAQ). Shiu et al.'s
  notebooks predate that and call the GRN groups "right hemisphere" and 720575940660219265 "MN9 left" in
  the image convention; the annotations say `left` for all annotated sugar, bitter and Ir94e GRNs and
  `right` for that MN9. Both conventions agree that the GRNs and that MN9 are contralateral. The registry
  descriptions and a model fact state this, so the digest no longer reads as a conflict.
- `registry/readout_proxies.json` holds one proxy, `mn9_rostrum_extension` (Shiu et al. 2024, citing
  Gordon & Scott 2009 and McKellar et al. 2020). Its numbers were checked against the paper: 11 of 106 cell
  types predicted to make MN9 fire at 50 Hz, 10 of them elicited rostrum extension, 4 of the 95 predicted
  not to did; 101 of 106 correct (95.3 %), reported as ">90 % accuracy" (Fig. 2a-c). The contralateral MN9
  bias is a model result (Fig. 1c, Extended Data Fig. 1d). A test recomputes every "X of N (P %)" pair. No other readout behaviour is stated in that paper with
  neuron ids in this registry (the antennal grooming neurons aBN1/aBN2/aDN1/aDN2 are not registered).
