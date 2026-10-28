package training

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/policymlp"
)

type command struct {
	Cmd             string   `json:"cmd"`
	Configs         []Config `json:"configs"`
	Indices         []int    `json:"indices"`
	EnemyActions    [][]int  `json:"enemy_actions"`
	DefenderActions []int    `json:"defender_actions"`
}

type ArrayDescriptor struct {
	Name   string `json:"name"`
	DType  string `json:"dtype"`
	Shape  []int  `json:"shape"`
	NBytes int    `json:"nbytes"`
	data   []byte
}

type Header struct {
	Type                       string            `json:"type"`
	Protocol                   int               `json:"protocol"`
	ObservationVersion         int               `json:"observation_version"`
	NormalizationVersion       int               `json:"normalization_version"`
	DefenderObservationVersion int               `json:"defender_observation_version"`
	EnemySlots                 int               `json:"enemy_slots"`
	ObservationSize            int               `json:"observation_size"`
	ActionCount                int               `json:"action_count"`
	CriticSize                 int               `json:"critic_size"`
	Arrays                     []ArrayDescriptor `json:"arrays"`
	Infos                      []Info            `json:"infos"`
	Error                      string            `json:"error,omitempty"`
	EnemyPolicy                *EnemyPolicyInfo  `json:"enemy_policy,omitempty"`
}

// EnemyPolicyInfo identifies the exact native actor used for -1 enemy actions.
// It is supplied in the hello header only; the model cannot change mid-worker.
type EnemyPolicyInfo struct {
	Implementation string             `json:"implementation"`
	Metadata       policymlp.Metadata `json:"metadata"`
}

func header(kind string) Header {
	return Header{Type: kind, Protocol: 1, ObservationVersion: game.ObservationVersion, NormalizationVersion: game.NormalizationVersion,
		DefenderObservationVersion: 1, EnemySlots: game.DecisionSlots, ObservationSize: game.ObservationSize, ActionCount: game.ActionCount,
		CriticSize: CriticSize, Arrays: []ArrayDescriptor{}, Infos: []Info{}}
}

// Serve is a synchronous framed IPC loop. No logger or test output may write to
// out: a single diagnostic byte would corrupt every following binary response.
func Serve(in io.Reader, out io.Writer) error {
	return ServeWithEnemyModel(in, out, nil)
}

// ServeWithEnemyModel evaluates the artifact's own sampler and seeded per-tank
// RNG through the production Policy path. Python-supplied actions still retain
// their existing meaning; callers request native inference by sending -1.
// A nil model preserves the legacy protocol and scripted fallback exactly.
func ServeWithEnemyModel(in io.Reader, out io.Writer, model *policymlp.Model) error {
	var batch Batch
	hello := header("hello")
	if model != nil {
		batch.policy = model
		hello.EnemyPolicy = &EnemyPolicyInfo{Implementation: "go-policymlp", Metadata: model.Metadata()}
	}
	defer batch.Close()
	writer := bufio.NewWriterSize(out, 128*1024)
	if err := writeHeader(writer, hello); err != nil {
		return err
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var cmd command
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&cmd)
		if err == nil {
			var extra any
			if tail := decoder.Decode(&extra); tail != io.EOF {
				err = fmt.Errorf("command must contain exactly one JSON value")
			}
		}
		var states []Transition
		if err == nil {
			switch cmd.Cmd {
			case "reset":
				states, err = batch.Reset(cmd.Configs)
			case "reset_indices":
				states, err = batch.ResetIndices(cmd.Indices, cmd.Configs)
			case "step":
				if len(cmd.EnemyActions) != batch.Len() || len(cmd.DefenderActions) != batch.Len() || batch.Len() == 0 {
					err = fmt.Errorf("enemy_actions and defender_actions must match the nonempty batch")
					break
				}
				actions := make([]Actions, batch.Len())
				for i, row := range cmd.EnemyActions {
					if len(row) != game.DecisionSlots {
						err = fmt.Errorf("enemy_actions[%d] must contain %d actions", i, game.DecisionSlots)
						break
					}
					copy(actions[i].Enemy[:], row)
					actions[i].Defender = cmd.DefenderActions[i]
				}
				if err == nil {
					states, err = batch.Step(actions)
				}
			case "close":
				batch.Close()
				return writeHeader(writer, header("closed"))
			default:
				err = fmt.Errorf("unknown cmd %q", cmd.Cmd)
			}
		}
		if err != nil {
			h := header("error")
			h.Error = err.Error()
			if err = writeHeader(writer, h); err != nil {
				return err
			}
			continue
		}
		if err = writeHeader(writer, transitionHeader(states)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func writeHeader(writer *bufio.Writer, h Header) error {
	if err := json.NewEncoder(writer).Encode(h); err != nil {
		return err
	}
	for _, array := range h.Arrays {
		if _, err := writer.Write(array.data); err != nil {
			return err
		}
	}
	return writer.Flush()
}
func floatArray(name string, shape []int, data []float32) ArrayDescriptor {
	payload := make([]byte, len(data)*4)
	for i, v := range data {
		binary.LittleEndian.PutUint32(payload[i*4:], math.Float32bits(v))
	}
	return ArrayDescriptor{Name: name, DType: "float32", Shape: shape, NBytes: len(payload), data: payload}
}
func byteArray(name string, shape []int, data []byte) ArrayDescriptor {
	return ArrayDescriptor{Name: name, DType: "uint8", Shape: shape, NBytes: len(data), data: data}
}
func intArray(name string, shape []int, data []int32) ArrayDescriptor {
	payload := make([]byte, len(data)*4)
	for i, v := range data {
		binary.LittleEndian.PutUint32(payload[i*4:], uint32(v))
	}
	return ArrayDescriptor{Name: name, DType: "int32", Shape: shape, NBytes: len(payload), data: payload}
}
func bit(b bool) byte {
	if b {
		return 1
	}
	return 0
}
func transitionHeader(states []Transition) Header {
	n := len(states)
	h := header("transition")
	h.Infos = make([]Info, n)
	enemyObs := make([]float32, 0, n*5*320)
	enemyMask := make([]byte, 0, n*5*10)
	enemyActive := make([]byte, 0, n*5)
	enemyIDs := make([]int32, 0, n*5)
	defenderObs := make([]float32, 0, n*320)
	defenderMask := make([]byte, 0, n*10)
	defenderActive := make([]byte, 0, n)
	criticObs := make([]float32, 0, n*CriticSize)
	enemyReward := make([]float32, 0, n)
	defenderReward := make([]float32, 0, n)
	terminated := make([]byte, 0, n)
	truncated := make([]byte, 0, n)
	for i, t := range states {
		h.Infos[i] = t.Info
		for slot := 0; slot < game.DecisionSlots; slot++ {
			o := t.Frame.Observations[slot]
			enemyObs = append(enemyObs, o.Values[:]...)
			for _, m := range o.Mask {
				enemyMask = append(enemyMask, bit(m))
			}
			enemyActive = append(enemyActive, bit(t.Frame.Active[slot]))
			enemyIDs = append(enemyIDs, int32(t.Frame.EnemyIDs[slot]))
		}
		do, dm, da := DefenderObservation(t)
		defenderObs = append(defenderObs, do[:]...)
		for _, m := range dm {
			defenderMask = append(defenderMask, bit(m))
		}
		defenderActive = append(defenderActive, bit(da))
		co := CriticObservation(t)
		criticObs = append(criticObs, co[:]...)
		enemyReward = append(enemyReward, t.EnemyReward)
		defenderReward = append(defenderReward, t.DefenderReward)
		terminated = append(terminated, bit(t.Terminated))
		truncated = append(truncated, bit(t.Truncated))
	}
	h.Arrays = []ArrayDescriptor{
		floatArray("enemy_obs", []int{n, 5, 320}, enemyObs), byteArray("enemy_mask", []int{n, 5, 10}, enemyMask),
		byteArray("enemy_active", []int{n, 5}, enemyActive), intArray("enemy_ids", []int{n, 5}, enemyIDs),
		floatArray("defender_obs", []int{n, 320}, defenderObs), byteArray("defender_mask", []int{n, 10}, defenderMask), byteArray("defender_active", []int{n}, defenderActive),
		floatArray("critic_obs", []int{n, CriticSize}, criticObs), floatArray("enemy_reward", []int{n}, enemyReward), floatArray("defender_reward", []int{n}, defenderReward),
		byteArray("terminated", []int{n}, terminated), byteArray("truncated", []int{n}, truncated),
	}
	return h
}
