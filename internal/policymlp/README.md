# Tiny actor deployment contract

This package evaluates a pre-exported actor in pure Go for gameplay, artifact
validation, cross-language parity and performance measurements. Qualified
weights are bundled separately in [models/](../../models/README.md).
`internal/difficulty` loads and pins all three release models before server
startup, rejecting untrained, missing or changed artifacts. This low-level
adapter also accepts explicit synthetic fixtures for parity tests and uses a
bounded scripted fallback for numerical/observation failure during a decision.

## Artifact

The artifact directory contains two regular files, `metadata.json` (at most 16 KiB) and `weights.bin` (at most 250,000 bytes). Symlink files are rejected. Metadata requires all keys below, rejects unknown/duplicate keys, and accepts no trailing JSON value.

```json
{
  "schemaVersion": 1,
  "modelName": "enemy-tiny-mlp",
  "policyVersion": "experiment-001",
  "difficulty": "trainee",
  "trainingSeed": 42,
  "trained": false,
  "observationVersion": 2,
  "normalizationVersion": 2,
  "inputSize": 320,
  "hiddenSizes": [64, 32],
  "outputSize": 10,
  "activation": "relu",
  "actionEncoding": "move*2+fire",
  "decisionHz": 10,
  "observationDelayDecisions": 2,
  "weightsFile": "weights.bin",
  "weightsFormat": "float32-le",
  "weightsLayout": "out-in-row-major-weights-then-bias",
  "weightsSHA256": "REPLACE_WITH_THE_64_CHARACTER_LOWERCASE_SHA256_OF_WEIGHTS_BIN",
  "sampler": {
    "mode": "argmax",
    "temperature": 1.0,
    "rng": "xorshift64-v1"
  }
}
```

The checksum shown is a placeholder, not a valid artifact. `trained:false`
identifies random/synthetic parity fixtures; the application's startup loader
rejects these. `trained:true` is exporter provenance, not a quality claim;
evaluate a checkpoint before deploying it. Model/version names use 1–64 ASCII
letters, digits, `.`, `_`, or `-`. `trainingSeed` is a nonnegative signed 64-bit
integer. `difficulty` must be exactly `trainee`, `expert`, or `ace`; each has an
independent trained artifact. Difficulty selection and score multipliers are
handled by `internal/server` and `internal/difficulty`, outside this adapter.

Allowed production architectures are `320→64→32→10` and `320→32→16→10`. Larger teacher models must be distilled/exported to one of these shapes. Hidden layers use ReLU and the final layer outputs raw logits.

For each of the three Linear layers, `weights.bin` stores all `W[out,input]` values row-major, followed by `bias[out]`; concatenate layers in forward order. Each parameter is IEEE-754 float32 in little-endian order. The 64/32 actor has 22,954 parameters (91,816 bytes); the 32/16 actor has 10,970 parameters (43,880 bytes). The loader checks exact length, SHA256, metadata compatibility, and every parameter's finiteness.

The Go dot-product loop begins with bias, traverses input columns in increasing order, and rounds each multiplication and addition to float32. Compare PyTorch/BLAS logits with an appropriate float32 tolerance because reduction orders can differ. ReLU follows each hidden layer. No automatic observation normalization or clipping is applied in valid inference: use the engine's version-2 observation exactly.

## Masks and sampling

Observations contain 320 finite values in `[-1,1]`, including legal-action bits at indices `298:308`. Those entries must be exactly 0/1 and agree with the separate ten-element mask. At least one action must be legal. The action index is `move*2 + fire`, where move is `0=idle, 1=up, 2=right, 3=down, 4=left`.

For every valid decision, both sampling modes increment `PolicyMemory.Decisions` and advance `PolicyMemory.Random` once. If random is zero, replace it with `0x9e3779b97f4a7c15`; then perform unsigned 64-bit wrapping operations:

```text
x ^= x << 13
x ^= x >> 7
x ^= x << 17
u = float64(x >> 11) / 9007199254740992
```

- `argmax` picks the largest legal logit, breaking ties toward the lowest action index.
- `categorical` computes `exp((float64(logit)-maxLegalLogit)/temperature)` for legal actions, using float64 arithmetic. It picks the first legal action whose cumulative weight is strictly greater than `u * sum(weights)`. Temperature must be finite and positive. Illegal actions receive zero probability.
- Per-tank RNG memory is private. The actor has no mutable shared weights, RNG, cache, or scratch buffers. Sharing a model across rooms is safe.
- Invalid observation values, incompatible versions, inconsistent/all-false masks, or nonfinite inference output activate the scripted fallback. Nonfinite input features are zeroed before fallback and other values are bounded to `[-1,1]`. The selected fallback action is checked against the supplied mask; an all-false mask yields idle. These corruption paths are not expected during valid engine operation.

The engine already applies the two-decision perception delay. The actor must not delay the observation again.

## Cross-language parity CLI

```sh
go run ./cmd/policycheck -model /path/to/artifact < corpus.jsonl
```

Each input line is an independent request:

```text
{"version":2,"observation":[320 floats],"mask":[10 booleans],"random":"42","decisions":0}
```

`random` is a decimal string to preserve all uint64 bits. Omitted `random` and `decisions` mean zero. Output contains `logits`, `actionIndex`, `move`, `fire`, updated `random` as a decimal string, and updated `decisions`. Invalid lines produce an `error`/`line` object and a nonzero final exit status. Model-load diagnostics go to stderr. This tooling path intentionally accepts explicitly untrained artifacts.

Optional `-sampler argmax|categorical` and `-temperature NUMBER` override sampling in the parity tool. Deployment uses the sampler recorded in metadata.

```sh
go run ./cmd/policycheck -model /path/to/artifact -benchmark -iterations 100000
go test ./internal/policymlp -run '^$' -bench Decide -benchmem
```

The CLI reports local elapsed time, allocations, and arithmetic projected cost at 1,000 decisions/second. It excludes observation construction, simulation and networking; it is not proof of Linux CPU-quota compliance. The tests assert zero per-`Decide` heap allocations for both modes and exercise shared-model concurrency under the race detector.
