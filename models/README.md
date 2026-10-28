# Qualified native-v3 models

These are the exact deployment actors from the qualified `release-native-v3`
release. Each model has 22,954 parameters, with 91,816 bytes of FP32 weights.
The three models together need 275,448 bytes of weights. They execute directly
in Go without Python, ONNX, GPU, ANE, or an inference service.

| Difficulty | Policy version | Categorical temperature | Score multiplier |
| --- | --- | --- | --- |
| Trainee | `nwt-trainee-v3` | 1.0 | 1.0 |
| Expert | `nwt-expert-v3` | 0.3 | 1.5 |
| Ace | `nwt-ace-v3` | 0.5 | 2.0 |

`metadata.json` contains the SHA-256 of `weights.bin`, the observation contract,
and the sampler. Startup also checks the release pins in `internal/difficulty`.
Do not replace the categorical sampler with argmax, change temperatures, or
substitute a training checkpoint: the qualification covers these exact weights
and samplers. The older Ace T0.3 candidate is not part of this release.

All actors use observation/normalization v2, `320 → 64 → 32 → 10` ReLU,
`move*2+fire`, 10 Hz decisions, a two-decision observation delay, and
`xorshift64-v1` RNG. The models are loaded once, while each tank keeps its own
`PolicyMemory`.

Each tier passed 600 fresh evaluation matches across ten levels. Reference
defender win rates were 74.5%, 48.75%, and 26.75% respectively. These are fixed
bot results, not claims about human win rates or enjoyment.

The release previously passed a bounded 25-room simulation/inference/JSON probe
on the target host. That probe excludes the full HTTP/WebSocket application;
it is not a production deployment or a sustained full-server load test.

Licensed under the repository's MIT License.
