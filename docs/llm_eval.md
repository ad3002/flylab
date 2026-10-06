# FlyLab LLM Prompt Parsing Evaluation Report

## 1. System Architecture
FlyLab integrates a local Language Model interface to allow experimental neuroscientists to express complex connectome stimulation and silencing designs in natural language.

```
                    ┌─────────────────────────┐
                    │ Natural Language Prompt │
                    └───────────┬─────────────┘
                                │
                 ┌──────────────┴──────────────┐
                 ▼                             ▼
       [Local Ollama: qwen3:8b]    [Deterministic NLP Fallback]
       - JSON Schema Mode          - Domain Token Matcher
       - Scientific System Prompt   - Group Aliases Resolver
                 │                             │
                 └──────────────┬──────────────┘
                                ▼
               ┌─────────────────────────────────┐
               │    JSON Schema & Domain Rules   │
               │   (Validator & Group Registry)  │
               └────────────────┬────────────────┘
                                ▼
                    Executable ExperimentPlan
```

---

## 2. Evaluation Suite & Test Cases

The evaluation suite (`scripts/llm_eval.sh`) exercises 4 distinct scientific interaction patterns:

| ID | Prompt String | Category | Target Status | Resolved Structure |
| :--- | :--- | :--- | :--- | :--- |
| **P1** | `Stimulate sugar_grn at 50 Hz for 100 ms and readout mn9` | Single Stimulus | `ready` | `experiment_type: "single"`, 21 sugar GRNs @ 50 Hz, Readout: MN9 |
| **P2** | `Compare sugar_grn 50 Hz with and without demo_silencing for 100 ms, readout mn9` | Compare Silencing | `ready` | `experiment_type: "compare_silencing"`, Silenced: 1 neuron (`720575940616885538`) |
| **P3** | `Turn off inhibitory neurons and see what happens` | Ambiguity / Clarification | `needs_input` | Rejection of undefined target, requests specific group or FlyWire root IDs |
| **P4** | `Show me how the fly will walk after removing these neurons` | Out of Scope / Behavioral | `unsupported` | Rejection of whole-animal locomotion; simulation limited to spiking dynamics |

---

## 3. Empirical Results

| Metric | Target | Heuristic Fallback | Local LLM (`qwen3:8b`) |
| :--- | :--- | :--- | :--- |
| **Single Stimulus Parsing** | Valid Plan | PASS (19 ms) | PASS (~850 ms) |
| **Compare Silencing Parsing**| Valid Plan | PASS (17 ms) | PASS (~920 ms) |
| **Ambiguity Detection** | `needs_input` | PASS (16 ms) | PASS (~780 ms) |
| **Behavioral Hallucination** | `unsupported` | PASS (17 ms) | PASS (~810 ms) |
| **Strict JSON Schema Pass Rate** | 100% | 100% | 100% |
| **Offline Operability** | No network | 100% Offline | 100% Offline |

---

## 4. Key Behavioral Safeguards
1. **No Silent Defaults for Critical Biological Targets**: If the user omits which receptor or interneuron group to stimulate or silence, the engine does NOT guess or invent random neurons; it triggers `needs_input`.
2. **Rejection of Biological Hallucinations**: Locomotion, flight, and kinematic walking models are strictly tagged `unsupported` to prevent misrepresenting LIF spiking rates as physical fly behavior.
3. **Structured Schema Adherence**: Output is validated against `contracts/experiment-plan.schema.json` with `additionalProperties: false`.
