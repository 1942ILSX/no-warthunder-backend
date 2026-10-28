// Package policymlp loads the versioned, tiny deployment actor exported by the
// offline trainer. It has no inference dependencies, CGo, sockets, or mutable
// shared inference state. All rooms share immutable weights; each tank owns its
// game.PolicyMemory and every Decide scratch buffer lives on the stack.
package policymlp

import (
	"BattleCityReforge/internal/game"
	"errors"
	"math"
)

const (
	SchemaVersion         = 1
	WeightsFormat         = "float32-le"
	WeightsLayout         = "out-in-row-major-weights-then-bias"
	ActionEncoding        = "move*2+fire"
	SamplerRNG            = "xorshift64-v1"
	DefaultRandom  uint64 = 0x9e3779b97f4a7c15
	MaskOffset            = 298
)

var (
	ErrObservationVersion = errors.New("policy observation version does not match model")
	ErrObservationValue   = errors.New("policy observation must contain finite normalized values in [-1,1]")
	ErrActionMask         = errors.New("policy mask must match observation mask features and contain a legal action")
	ErrNonfiniteLogits    = errors.New("policy inference produced nonfinite activations or logits")
)

type Sampler struct {
	Mode        string  `json:"mode"`
	Temperature float64 `json:"temperature"`
	RNG         string  `json:"rng"`
}

// Metadata is intentionally strict. A changed normalization, action encoding,
// or delay requires an explicit new contract; silently loading it is unsafe.
// Trained is provenance supplied by the exporter, not a quality certificate.
type Metadata struct {
	SchemaVersion             int     `json:"schemaVersion"`
	ModelName                 string  `json:"modelName"`
	PolicyVersion             string  `json:"policyVersion"`
	Difficulty                string  `json:"difficulty"`
	TrainingSeed              int64   `json:"trainingSeed"`
	Trained                   bool    `json:"trained"`
	ObservationVersion        int     `json:"observationVersion"`
	NormalizationVersion      int     `json:"normalizationVersion"`
	InputSize                 int     `json:"inputSize"`
	HiddenSizes               []int   `json:"hiddenSizes"`
	OutputSize                int     `json:"outputSize"`
	Activation                string  `json:"activation"`
	ActionEncoding            string  `json:"actionEncoding"`
	DecisionHz                int     `json:"decisionHz"`
	ObservationDelayDecisions int     `json:"observationDelayDecisions"`
	WeightsFile               string  `json:"weightsFile"`
	WeightsFormat             string  `json:"weightsFormat"`
	WeightsLayout             string  `json:"weightsLayout"`
	WeightsSHA256             string  `json:"weightsSHA256"`
	Sampler                   Sampler `json:"sampler"`
}

type layer struct {
	input, output int
	weights, bias []float32
}

type Model struct {
	metadata Metadata
	layers   [3]layer
	name     string
}

var _ game.Policy = (*Model)(nil)

func (model *Model) Name() string { return model.name }

// Metadata returns a copy so a caller cannot mutate a shared model's shape.
func (model *Model) Metadata() Metadata {
	metadata := model.metadata
	metadata.HiddenSizes = append([]int(nil), metadata.HiddenSizes...)
	return metadata
}

// WithSampler makes a new model handle sharing the same immutable weights. It
// is used by parity tooling; normal deployment honors the artifact sampler.
func (model *Model) WithSampler(mode string, temperature float64) (*Model, error) {
	copy := *model
	if mode != "" {
		copy.metadata.Sampler.Mode = mode
	}
	if temperature != 0 {
		copy.metadata.Sampler.Temperature = temperature
	}
	if err := validateSampler(copy.metadata.Sampler); err != nil {
		return nil, err
	}
	return &copy, nil
}

// Logits returns unmasked logits. No softmax is applied by the MLP. Each dot
// product explicitly rounds the multiplication and accumulation to float32,
// preventing fused multiply-add from changing the reference evaluation order.
// PyTorch BLAS output may still differ slightly due to its reduction order.
func (model *Model) Logits(observation game.Observation) ([game.ActionCount]float32, error) {
	var result [game.ActionCount]float32
	if err := validateObservation(observation, model.metadata.ObservationVersion); err != nil {
		return result, err
	}
	var first [64]float32
	var second [32]float32
	if !linear(model.layers[0], observation.Values[:], first[:], true) ||
		!linear(model.layers[1], first[:], second[:], true) ||
		!linear(model.layers[2], second[:], result[:], false) {
		return result, ErrNonfiniteLogits
	}
	return result, nil
}

func linear(layer layer, input, output []float32, relu bool) bool {
	for row := 0; row < layer.output; row++ {
		sum := layer.bias[row]
		weights := layer.weights[row*layer.input : (row+1)*layer.input]
		for column, weight := range weights {
			product := float32(weight * input[column])
			sum = float32(sum + product)
		}
		if !finite32(sum) {
			return false
		}
		if relu && sum < 0 {
			sum = 0
		}
		output[row] = sum
	}
	return true
}

func (model *Model) Decide(observation game.Observation, memory *game.PolicyMemory) game.Action {
	if memory == nil {
		return game.Action{}
	}
	logits, err := model.Logits(observation)
	if err != nil {
		return fallback(observation, memory)
	}
	return model.action(logits, observation.Mask, memory)
}

// Evaluate is the parity-tool path: it reports a malformed observation or
// numerical failure explicitly. Production Decide uses the scripted fallback.
func (model *Model) Evaluate(observation game.Observation, memory *game.PolicyMemory) ([game.ActionCount]float32, game.Action, error) {
	logits, err := model.Logits(observation)
	if err != nil {
		return logits, game.Action{}, err
	}
	if memory == nil {
		return logits, game.Action{}, errors.New("policy memory must not be nil")
	}
	return logits, model.action(logits, observation.Mask, memory), nil
}

func (model *Model) action(logits [game.ActionCount]float32, mask [game.ActionCount]bool, memory *game.PolicyMemory) game.Action {
	// Advancing once even for argmax gives both modes an explicit reproducible
	// memory contract. Zero is replaced before xorshift to avoid its fixed point.
	memory.Random = NextRandom(memory.Random)
	memory.Decisions++
	best := -1
	for index, legal := range mask {
		if legal && (best < 0 || logits[index] > logits[best]) {
			best = index
		}
	}
	chosen := best
	if model.metadata.Sampler.Mode == "categorical" {
		var probabilities [game.ActionCount]float64
		total := 0.0
		for index, legal := range mask {
			if legal {
				probabilities[index] = math.Exp((float64(logits[index]) - float64(logits[best])) / model.metadata.Sampler.Temperature)
				total += probabilities[index]
			}
		}
		threshold := float64(memory.Random>>11) * (1.0 / 9007199254740992.0) * total
		cumulative := 0.0
		for index, legal := range mask {
			if legal {
				chosen = index // Last legal action handles the final rounding edge.
				cumulative += probabilities[index]
				if threshold < cumulative {
					break
				}
			}
		}
	}
	action := game.ActionFromIndex(chosen)
	memory.LastAction = action
	return action
}

// NextRandom is shared with the documented Python parity implementation.
// Arithmetic wraps modulo 2^64. The sampler uniform is (result >> 11) / 2^53.
func NextRandom(value uint64) uint64 {
	if value == 0 {
		value = DefaultRandom
	}
	value ^= value << 13
	value ^= value >> 7
	value ^= value << 17
	return value
}

func validateObservation(observation game.Observation, version int) error {
	if observation.Version != version {
		return ErrObservationVersion
	}
	for _, value := range observation.Values {
		if !finite32(value) || value < -1.000001 || value > 1.000001 {
			return ErrObservationValue
		}
	}
	legal := false
	for index, allowed := range observation.Mask {
		expected := float32(0)
		if allowed {
			expected = 1
			legal = true
		}
		if observation.Values[MaskOffset+index] != expected {
			return ErrActionMask
		}
	}
	if !legal {
		return ErrActionMask
	}
	return nil
}

func fallback(observation game.Observation, memory *game.PolicyMemory) game.Action {
	for index, value := range observation.Values {
		if !finite32(value) {
			observation.Values[index] = 0
		} else {
			observation.Values[index] = max(-1, min(1, value))
		}
	}
	for index, legal := range observation.Mask {
		observation.Values[MaskOffset+index] = 0
		if legal {
			observation.Values[MaskOffset+index] = 1
		}
	}
	action := (game.ScriptedPolicy{}).Decide(observation, memory)
	if !observation.Mask[action.Index()] {
		action = game.Action{}
		for index, legal := range observation.Mask {
			if legal {
				action = game.ActionFromIndex(index)
				break
			}
		}
	}
	memory.LastAction = action
	return action
}

func finite32(value float32) bool {
	bits := math.Float32bits(value)
	return bits&0x7f800000 != 0x7f800000
}
