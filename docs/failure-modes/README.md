# Failure-mode catalog

Named, numbered infrastructure failure modes — each with the signature that
identifies it, the arithmetic that corroborates it, the fix that resolves it,
and a reproduction anyone can run.

Diagnosis is not observation. A dashboard shows that latency spiked; these
entries say *which mechanism did it and what to change*. Every entry here was
diagnosed on real hardware during [Velox's](https://github.com/getvelox/velox)
engineering work, and carries its evidence rather than asking for trust.

## Entry format

| Section | What it holds |
|---|---|
| **Symptom** | What it looks like from outside, so a sufferer recognises it |
| **Mechanism** | Why it happens, from first principles |
| **Signature** | The observations that distinguish it from lookalikes — coincidence, negative coincidence, arithmetic |
| **Fix** | A formula parameterised by *your* measurements, not a generic knob |
| **Does not claim** | The limits of the entry: what it is not, where it does not apply |
| **Reproduce** | How to induce the failure yourself and watch the fix work |

## Entries

| ID | Failure mode | Layer |
|---|---|---|
| [RFM-001](rfm-001-wal-pool-exhaustion.md) | WAL recycled-pool exhaustion | PostgreSQL write path |

More entries land as they are diagnosed and reproduced. An entry without a
reproduction does not ship.
