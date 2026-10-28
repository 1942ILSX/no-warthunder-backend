package policymlp

import (
	"BattleCityReforge/internal/game"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxMetadataBytes = 16 << 10
	MaxWeightsBytes  = 250000
)

// Load validates an entire artifact before exposing any model. It accepts
// trained:false artifacts for parity tests. The application's difficulty loader
// rejects those and pins qualified weights plus their exact sampler settings.
func Load(directory string) (*Model, error) {
	metadataBytes, err := boundedFile(filepath.Join(directory, "metadata.json"), MaxMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("policy metadata: %w", err)
	}
	metadata, err := decodeMetadata(metadataBytes)
	if err != nil {
		return nil, err
	}
	weightsBytes, err := boundedFile(filepath.Join(directory, metadata.WeightsFile), MaxWeightsBytes)
	if err != nil {
		return nil, fmt.Errorf("policy weights: %w", err)
	}
	sizes := [4]int{metadata.InputSize, metadata.HiddenSizes[0], metadata.HiddenSizes[1], metadata.OutputSize}
	count := 0
	for layer := 0; layer < 3; layer++ {
		count += sizes[layer+1] * (sizes[layer] + 1)
	}
	if len(weightsBytes) != count*4 {
		return nil, fmt.Errorf("policy weights: got %d bytes, require exactly %d", len(weightsBytes), count*4)
	}
	digest := sha256.Sum256(weightsBytes)
	if hex.EncodeToString(digest[:]) != metadata.WeightsSHA256 {
		return nil, errors.New("policy weights: SHA256 checksum mismatch")
	}
	weights := make([]float32, count)
	for index := range weights {
		weights[index] = math.Float32frombits(binary.LittleEndian.Uint32(weightsBytes[index*4 : index*4+4]))
		if !finite32(weights[index]) {
			return nil, fmt.Errorf("policy weights: parameter %d is nonfinite", index)
		}
	}
	model := &Model{metadata: metadata, name: metadata.ModelName + "/" + metadata.PolicyVersion}
	offset := 0
	for index := range model.layers {
		input, output := sizes[index], sizes[index+1]
		next := offset + input*output
		model.layers[index] = layer{input: input, output: output, weights: weights[offset:next], bias: weights[next : next+output]}
		offset = next + output
	}
	return model, nil
}

func boundedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("must be a regular file of at most %d bytes", limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

var metadataKeys = []string{
	"schemaVersion", "modelName", "policyVersion", "difficulty", "trainingSeed", "trained",
	"observationVersion", "normalizationVersion", "inputSize", "hiddenSizes", "outputSize",
	"activation", "actionEncoding", "decisionHz", "observationDelayDecisions",
	"weightsFile", "weightsFormat", "weightsLayout", "weightsSHA256", "sampler",
}

func decodeMetadata(data []byte) (Metadata, error) {
	var metadata Metadata
	if err := rejectDuplicateKeys(data); err != nil {
		return metadata, fmt.Errorf("policy metadata JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return metadata, fmt.Errorf("policy metadata JSON: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return metadata, err
	}
	for _, key := range metadataKeys {
		if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return metadata, fmt.Errorf("policy metadata: missing required field %q", key)
		}
	}
	var samplerFields map[string]json.RawMessage
	_ = json.Unmarshal(fields["sampler"], &samplerFields)
	for _, key := range []string{"mode", "temperature", "rng"} {
		if value, ok := samplerFields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return metadata, fmt.Errorf("policy metadata sampler: missing required field %q", key)
		}
	}
	if err := validateMetadata(metadata); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func validateMetadata(metadata Metadata) error {
	if metadata.SchemaVersion != SchemaVersion || metadata.ObservationVersion != game.ObservationVersion || metadata.NormalizationVersion != game.NormalizationVersion {
		return fmt.Errorf("policy metadata: require schema=%d observation=%d normalization=%d", SchemaVersion, game.ObservationVersion, game.NormalizationVersion)
	}
	if metadata.ObservationVersion != 2 || metadata.NormalizationVersion != 2 {
		return errors.New("policy adapter requires the version 2 observation and normalization contract")
	}
	if !validName(metadata.ModelName) || !validName(metadata.PolicyVersion) || metadata.TrainingSeed < 0 {
		return errors.New("policy metadata: modelName/policyVersion must be 1–64 ASCII letters, digits, dot, underscore or hyphen; trainingSeed must be nonnegative")
	}
	if metadata.Difficulty != "trainee" && metadata.Difficulty != "expert" && metadata.Difficulty != "ace" {
		return errors.New("policy metadata: difficulty must be trainee, expert, or ace")
	}
	if metadata.InputSize != game.ObservationSize || metadata.OutputSize != game.ActionCount || len(metadata.HiddenSizes) != 2 ||
		!((metadata.HiddenSizes[0] == 64 && metadata.HiddenSizes[1] == 32) || (metadata.HiddenSizes[0] == 32 && metadata.HiddenSizes[1] == 16)) {
		return errors.New("policy metadata: supported shapes are 320→64→32→10 and 320→32→16→10")
	}
	if metadata.Activation != "relu" || metadata.ActionEncoding != ActionEncoding || metadata.DecisionHz != game.DecisionHz || metadata.ObservationDelayDecisions != 2 {
		return errors.New("policy metadata: incompatible activation, action encoding, decision cadence or observation delay")
	}
	if metadata.WeightsFile != "weights.bin" || metadata.WeightsFormat != WeightsFormat || metadata.WeightsLayout != WeightsLayout {
		return errors.New("policy metadata: weights must use weights.bin, float32-le, and out-in-row-major-weights-then-bias")
	}
	digest, err := hex.DecodeString(metadata.WeightsSHA256)
	if err != nil || len(digest) != sha256.Size || metadata.WeightsSHA256 != strings.ToLower(metadata.WeightsSHA256) {
		return errors.New("policy metadata: weightsSHA256 must be 64 lowercase hexadecimal characters")
	}
	return validateSampler(metadata.Sampler)
}

func validateSampler(sampler Sampler) error {
	if (sampler.Mode != "argmax" && sampler.Mode != "categorical") || sampler.RNG != SamplerRNG || math.IsNaN(sampler.Temperature) || math.IsInf(sampler.Temperature, 0) || sampler.Temperature <= 0 {
		return errors.New("policy sampler: require argmax/categorical, positive finite temperature, and xorshift64-v1")
	}
	return nil
}

func validName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

// Reject duplicate object keys rather than silently taking the last contract
// declaration. Also rejects trailing JSON values and malformed nested values.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate or invalid object key %q", key)
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected closing delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}
