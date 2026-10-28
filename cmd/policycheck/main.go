// Command policycheck validates exported actor artifacts and evaluates a JSONL
// observation corpus for Python/Go parity. It accepts explicitly untrained
// artifacts so random test fixtures never need to masquerade as real policies.
package main

import (
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/policymlp"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"time"
)

type request struct {
	Version     int       `json:"version"`
	Observation []float32 `json:"observation"`
	Mask        []bool    `json:"mask"`
	Random      string    `json:"random"`
	Decisions   uint64    `json:"decisions"`
}

type response struct {
	Logits      [game.ActionCount]float32 `json:"logits"`
	ActionIndex int                       `json:"actionIndex"`
	Move        int                       `json:"move"`
	Fire        bool                      `json:"fire"`
	Random      string                    `json:"random"`
	Decisions   uint64                    `json:"decisions"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "policycheck:", err)
		os.Exit(1)
	}
}

func run() error {
	directory := flag.String("model", "", "directory containing metadata.json and weights.bin")
	mode := flag.String("sampler", "", "optional argmax or categorical override; default uses metadata")
	temperature := flag.Float64("temperature", 0, "optional positive sampler temperature override; 0 uses metadata")
	benchmark := flag.Bool("benchmark", false, "measure repeated Decide calls instead of reading JSONL")
	iterations := flag.Int("iterations", 100000, "benchmark decisions (1–10000000)")
	flag.Parse()
	if *directory == "" {
		return errors.New("-model is required")
	}
	model, err := policymlp.Load(*directory)
	if err != nil {
		return err
	}
	model, err = model.WithSampler(*mode, *temperature)
	if err != nil {
		return err
	}
	if *benchmark {
		if *iterations < 1 || *iterations > 10000000 {
			return errors.New("benchmark iterations must be between 1 and 10000000")
		}
		return runBenchmark(model, *iterations)
	}
	return runCorpus(model, os.Stdin, os.Stdout)
}

func runCorpus(model *policymlp.Model, input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 8192), 64<<10)
	encoder := json.NewEncoder(output)
	line, failures := 0, 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		result, err := evaluate(model, scanner.Bytes())
		if err != nil {
			failures++
			if err := encoder.Encode(map[string]any{"error": err.Error(), "line": line}); err != nil {
				return err
			}
			continue
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d corpus request(s) failed validation", failures)
	}
	return nil
}

func evaluate(model *policymlp.Model, data []byte) (response, error) {
	var result response
	var body request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return result, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return result, errors.New("expected one JSON object per line")
	}
	if len(body.Observation) != game.ObservationSize || len(body.Mask) != game.ActionCount {
		return result, errors.New("require exactly 320 observation values and 10 mask booleans")
	}
	observation := game.Observation{Version: body.Version}
	copy(observation.Values[:], body.Observation)
	copy(observation.Mask[:], body.Mask)
	memory := game.PolicyMemory{Decisions: body.Decisions}
	if body.Random != "" {
		random, err := strconv.ParseUint(body.Random, 10, 64)
		if err != nil {
			return result, errors.New("random must be a decimal uint64 string")
		}
		memory.Random = random
	}
	logits, action, err := model.Evaluate(observation, &memory)
	if err != nil {
		return result, err
	}
	return response{Logits: logits, ActionIndex: action.Index(), Move: action.Move, Fire: action.Fire, Random: strconv.FormatUint(memory.Random, 10), Decisions: memory.Decisions}, nil
}

var benchmarkAction game.Action

func runBenchmark(model *policymlp.Model, iterations int) error {
	observation := game.Observation{Version: game.ObservationVersion}
	for index := range observation.Values {
		observation.Values[index] = float32(index%17) / 17
	}
	for index := range observation.Mask {
		observation.Mask[index] = true
		observation.Values[policymlp.MaskOffset+index] = 1
	}
	memory := game.PolicyMemory{Random: 42}
	// Warm the loaded actor and grow the goroutine stack before measuring. GC is
	// not disabled: the report is deliberately an ordinary local-process sample.
	for i := 0; i < 1000; i++ {
		benchmarkAction = model.Decide(observation, &memory)
	}
	if _, err := model.Logits(observation); err != nil {
		return fmt.Errorf("benchmark observation failed: %w", err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	for i := 0; i < iterations; i++ {
		benchmarkAction = model.Decide(observation, &memory)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	nsPerDecision := float64(elapsed.Nanoseconds()) / float64(iterations)
	metadata := model.Metadata()
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"model": model.Name(), "difficulty": metadata.Difficulty, "trained": metadata.Trained, "sampler": metadata.Sampler,
		"iterations": iterations, "elapsedMs": float64(elapsed) / float64(time.Millisecond),
		"nsPerDecision": nsPerDecision, "allocations": after.Mallocs - before.Mallocs,
		"allocatedBytes":                           after.TotalAlloc - before.TotalAlloc,
		"allocationsPerDecision":                   float64(after.Mallocs-before.Mallocs) / float64(iterations),
		"projectedSecondsPerSecondAt1000Decisions": nsPerDecision * 1000 / 1e9,
		"measurementNote":                          "Local wall-clock microbenchmark of Decide only; excludes observation construction, simulation, networking and GC outside this loop. Projection is arithmetic throughput, not verified CPU usage or Linux quota compliance.",
	})
}
