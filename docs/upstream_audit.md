# Upstream Audit: Drosophila Brain Spiking Model

## 1. Upstream Source Repository

- **Repository**: [philshiu/Drosophila_brain_model](https://github.com/philshiu/Drosophila_brain_model)
- **Scientific Publication**: Shiu et al., *A Drosophila computational brain model reveals sensorimotor processing*, Nature 2024. [doi:10.1038/s41586-024-07763-9](https://doi.org/10.1038/s41586-024-07763-9)
- **Pinned Commit SHA**: `91bdd1e7dcf193f3e7ca5a8933497fcef63b7960` (September 14, 2024)
- **License**: MIT License (preserved in `third_party/upstream/LICENSE`)

## 2. Dataset Audit (FlyWire Materialization 630)

| File | SHA256 | Size (Bytes) | Row Count | Description |
|---|---|---|---|---|
| `2023_03_23_completeness_630_final.csv` | `e6b71e17671a9bdb05f55e4bc6774640a1418cb7a05125e0fc994ad40f9bfdfb` | 3,057,611 | 127,400 | Complete list of FlyWire neurons (FlyWire IDs as 64-bit integers, index 0 to 127,399). |
| `2023_03_23_connectivity_630_final.parquet` | `94db8c650533bc36ffa3223f2e62325d5648b8d6bd31c3a4e1c804628c7557b3` | 86,630,944 | 14,687,178 | Directed synaptic connectivity between FlyWire neuron pairs. |

### Connectivity Table Semantics
- Each row represents an **aggregated pair of presynaptic and postsynaptic neurons**, *not* individual anatomical synapses.
- The column `Connectivity` is the count of synapses between the pair.
- The column `Excitatory` is $+1$ for excitatory (cholinergic) and $-1$ for inhibitory (GABAergic / glutamatergic).
- The column `Excitatory x Connectivity` provides the signed integer multiplier applied to synaptic weight scale $w_{syn} = 0.275\text{ mV}$.
- Verified index bounds: `Presynaptic_Index` $\in [0, 127399]$, `Postsynaptic_Index` $\in [0, 127399]$.

## 3. Semantic Discrepancy: README vs Code Implementation

A key discrepancy between upstream documentation and code was discovered during the audit:

- **Upstream README claim**:
  > *"Silencing: In addition to activation, a different set of neurons can be silenced to model optogenetic silencing. This sets all synaptic connections to and from those neurons to zero."*
- **Actual Implementation in `model.py` (lines 124-125)**:
  ```python
  def silence(slnc, syn):
      for i in slnc:
          syn.w[' {} == i'.format(i)] = 0*mV
      return syn
  ```
  In Brian2, variable `i` refers exclusively to the **presynaptic** neuron index, while `j` refers to the postsynaptic neuron index. Therefore, `syn.w[' {} == i'.format(i)] = 0*mV` only zeroes **outgoing** synapses from neuron `i`. Ingoing synapses (where `j == i`) remain fully active and continue to deliver charges to the silenced neuron.

- **Project Architectural Decision**:
  We strictly faithfully follow the actual executable code of the published model (`model.py`), zeroing outgoing synapses. The UI and documentation explicitly communicate this behavior: *"Silencing zeroes outgoing synaptic transmissions from the targeted neurons"*.

## 4. Upstream Environment & Brian2 Integration

- **Python Runtime**: Python 3.10
- **Brian2 Version**: 2.5.1
- **Numerical Libraries**: NumPy 1.24/1.26, Pandas 2.x, PyArrow
- **Integration Step**: Default clock step $dt = 0.1\text{ ms}$ ($100\ \mu\text{s}$)
- **Integration Method**: Analytical linear solver (`method='linear'`) for the 2D linear ODE system $(v, g)$.
