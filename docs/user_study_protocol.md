# FlyLab User Study Protocol: Comparative Evaluation of Drosophila Brain Simulation Workflows

## 1. Study Objective
The objective of this user study is to evaluate the usability, operational efficiency, and error rates of three distinct workflows for conducting *in silico* Drosophila connectome experiments:
1. **Upstream Jupyter Notebook Baseline** (`example.ipynb` running Python / Brian2).
2. **FlyLab Parameter Form Interface** (web UI form with validation and preset groups).
3. **FlyLab Natural Language Interface** (local LLM-assisted prompt parsing).

---

## 2. Participant Cohort & Experimental Design

### Cohort
- **Target Size**: $N = 12$ participants (computational neuroscientists, bioinformatics graduate students, and systems biologists).
- **Inclusion Criteria**: Basic familiarity with neural circuits and Drosophila biology; no prior exposure to the FlyLab UI.

### Counterbalanced Within-Subject Design
To prevent order effects and skill acquisition bias from confounding tool efficiency, participants are assigned using a Latin square rotation across the three experimental modalities:

| Participant Group | Task 1 Modality | Task 2 Modality | Task 3 Modality |
| :--- | :--- | :--- | :--- |
| **Group 1** | Upstream Notebook | Web Form | Natural Language |
| **Group 2** | Web Form | Natural Language | Upstream Notebook |
| **Group 3** | Natural Language | Upstream Notebook | Web Form |

---

## 3. Standardized Evaluation Tasks

Each participant executes two standardized experimental protocols:

### Task A: Single Condition Baseline
- **Stimulus**: Activate labellar sugar gustatory receptor neurons (`sugar_grn`, 21 neurons) with 50 Hz Poisson input for 100 ms.
- **Readout**: Record and report firing rate of proboscis motor neuron pair (`mn9`).
- **Target Metric**: Verify baseline activation and check if MN9 fired.

### Task B: Optogenetic Ablation / Silencing Comparison
- **Stimulus**: Stimulate `sugar_grn` at 50 Hz for 100 ms.
- **Intervention**: Silence the primary sugar receptor (`demo_silencing`, neuron `720575940616885538`) in Condition B.
- **Readout**: Measure comparative change ($\Delta$ Hz) in downstream feeding motor neurons (`mn9`).
- **Deliverable**: Export verified result package and state the difference in spike counts between Baseline (A) and Silenced (B).

---

## 4. Quantitative Metrics

To cleanly dissociate cognitive effort from computational latency, three temporal components are tracked separately:

1. **Human Operator Time ($T_{\text{human}}$)**:
   - Time from task presentation until the participant initiates execution (typing code, selecting dropdowns, or drafting prompts).
2. **Computational Run Time ($T_{\text{compute}}$)**:
   - Server/runtime execution duration from process dispatch to result output.
3. **Total Turnaround Time ($T_{\text{total}} = T_{\text{human}} + T_{\text{compute}}$)**.
4. **Task Completion Rate**:
   - Percentage of participants successfully generating valid, scientifically verified results without fatal script errors or aborted runs.
5. **Parameter Error Rate**:
   - Frequency of invalid parameters (e.g., mismatched neuron IDs, reversed units, incorrect duration or rates).
6. **Intervention / Assistance Required**:
   - Number of facilitator hints required per task.

---

## 5. Qualitative Usability Assessment

Participants complete a standardized System Usability Scale (SUS) questionnaire and a task-specific qualitative survey upon completing each modality:

1. **Confidence in Results**: "I am confident that the simulation executed exactly the biological parameters I intended." (1–5 scale).
2. **Parameter Transparency**: "The software made it clear which neurons and synapses were affected." (1–5 scale).
3. **Replayability & Sharing**: "I can easily share these findings with colleagues for independent reproduction." (1–5 scale).

---

## 6. Data Collection Sheet Template

| Participant ID | Modality | Task | $T_{\text{human}}$ (s) | $T_{\text{compute}}$ (s) | $T_{\text{total}}$ (s) | Errors / Corrections | Success (Y/N) | Hints Needed |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| P01 | Notebook | Task A | | | | | | |
| P01 | Web Form | Task B | | | | | | |
| P01 | Natural Lang | Task A | | | | | | |

---

## 7. Reporting & Dissemination
Results will be tabulated in `docs/user_study_report.md` with mean $\pm$ standard error, ANOVA statistical significance across modalities, and qualitative user feedback to guide future UI/UX enhancements.
