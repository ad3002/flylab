# FlyLab LLM Prompt Parsing Evaluation Report

## 1. System Architecture
FlyLab turns a natural-language request into an executable `ExperimentPlan` with the Claude Code
CLI in print mode. The prompt goes to the CLI on stdin; the CLI returns a JSON envelope whose
`structured_output` must match a planner schema generated from the neuron registry and the plan
schema limits. The plan is then validated by the same validator as hand-written plans.

```
                    ┌─────────────────────────┐
                    │ Natural Language Prompt │
                    └───────────┬─────────────┘
                                ▼
   claude -p --model claude-sonnet-5-5 --tools "" --no-session-persistence
             --strict-mcp-config --setting-sources "" --output-format json
             --system-prompt <registry + limits> --json-schema <planner schema>
                                │
         ┌──────────────────────┼─────────────────────────────┐
         ▼                      ▼                             ▼
  status ready           needs_input / unsupported     CLI error, is_error, timeout,
  + plan                 (message, unresolved_fields)  malformed output, invalid plan
         │                      │                             │
         ▼                      ▼                             ▼
 JSON Schema & domain     returned to the user          502 LLM_ERROR (reason shown)
 rules (validator)
         ▼
 Executable ExperimentPlan

  claude binary not on PATH -> keyword parser, response carries llm_error (UI banner)
```

---

## 2. Evaluation Suite & Test Cases

The evaluation suite (`scripts/llm_eval.sh`) exercises these scientific interaction patterns (plus a FlyWire-mention case and a Russian compare-silencing case):

| ID | Prompt String | Category | Target Status | Resolved Structure |
| :--- | :--- | :--- | :--- | :--- |
| **P1** | `Stimulate sugar_grn at 50 Hz for 100 ms and readout mn9` | Single Stimulus | `ready` | `experiment_type: "single"`, 21 sugar GRNs @ 50 Hz, Readout: MN9 |
| **P2** | `Compare sugar_grn 50 Hz with and without demo_silencing for 100 ms, readout mn9` | Compare Silencing | `ready` | `experiment_type: "compare_silencing"`, Silenced: 1 neuron (`720575940616885538`) |
| **P3** | `Turn off inhibitory neurons and see what happens` | Ambiguity / Clarification | `needs_input` | Rejection of undefined target, requests specific group or FlyWire root IDs |
| **P4** | `Show me how the fly will walk after removing these neurons` | Out of Scope / Behavioral | `unsupported` | Rejection of whole-animal locomotion; simulation limited to spiking dynamics |

---

## 3. Empirical Results

`scripts/llm_eval.sh` runs the cases above (plus a FlyWire-mention case and a Russian
compare-silencing case) against a live server with the real CLI and fails unless every response
comes from `source: "claude"` with the expected status. The earlier local-model numbers no longer
apply and were removed.

Reference point (staging host, 2026-10-07, `TestRealClaudePlanner`): a compare-silencing
request was planned as `ready` in 4.3 s wall time (2.9 s reported by the CLI) at a cost of
USD 0.017 per parse.

Unit tests (`internal/llm`, `internal/api`) use a fake CLI (`internal/llm/testdata/fake_claude.sh`)
to cover success, `needs_input`, `unsupported`, `is_error`, non-zero exit, timeout, malformed or
missing `structured_output`, plans that fail validation, the missing-binary fallback, the
concurrency limit and the per-user rate limit.

---

## 4. Key Behavioral Safeguards
1. **No Silent Defaults for Critical Biological Targets**: If the user omits which receptor or interneuron group to stimulate or silence, the planner is instructed not to guess; it returns `needs_input` with the missing plan paths. Technical defaults (rate, duration, repeats, seed, MN9 readout) are allowed and named in the message.
2. **Rejection of Biological Hallucinations**: Locomotion, flight and other whole-animal behaviour is `unsupported`, so LIF spiking rates are not presented as physical fly behaviour. Mentioning the fly or FlyWire alone is not a reason to reject (the old keyword filter rejected any prompt containing "fly").
3. **Structured Schema Adherence**: Output is validated against `contracts/experiment-plan.schema.json` with `additionalProperties: false`.
4. **No Silent Fallback**: a failing planner is a visible `502 LLM_ERROR`; the keyword parser is used only when the CLI is not installed, and then the response carries `llm_error`.
