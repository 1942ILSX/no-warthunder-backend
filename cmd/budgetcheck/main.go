// budgetcheck is a bounded, standalone learned-policy resource probe. It does
// not import the HTTP server, listen on sockets, or write application data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/levels"
	"BattleCityReforge/internal/policymlp"
)

var profileNames = []string{"trainee", "expert", "ace"}

type configuration struct {
	Paths          [3]string
	Rooms          int
	Duration       time.Duration
	Scenario       string
	Profile        string
	Serialize      bool
	Seed           int64
	InferenceFloor int
	SetupPause     time.Duration
}

func parseConfiguration(args []string, output io.Writer) (configuration, error) {
	var c configuration
	fs := flag.NewFlagSet("budgetcheck", flag.ContinueOnError)
	fs.SetOutput(output)
	for i, name := range profileNames {
		fs.StringVar(&c.Paths[i], name, "", "strict trained "+name+" artifact directory")
	}
	fs.IntVar(&c.Rooms, "rooms", 25, "independent game rooms, 1–25")
	fs.DurationVar(&c.Duration, "duration", 30*time.Second, "paced measurement duration, 1s–60s")
	fs.StringVar(&c.Scenario, "scenario", "mixed", "mixed (levels1–10) or max-enemies (levels7–10, prefilled to4)")
	fs.StringVar(&c.Profile, "profile", "mixed", "mixed or trainee/expert/ace (all three artifacts still loaded)")
	fs.BoolVar(&c.Serialize, "serialize", true, "encode state JSON and round-trip input JSON at15Hz; discard output")
	fs.Int64Var(&c.Seed, "seed", 42, "deterministic scenario seed")
	fs.IntVar(&c.InferenceFloor, "inference-floor", 0, "optional combined live+supplemental inference floor per second: 0 or 1000")
	fs.DurationVar(&c.SetupPause, "setup-pause", 0, "context-cancellable pause before each initial room setup, 0–100ms; no effect on measured ticks or replacements")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if c.Rooms < 1 || c.Rooms > 25 || c.Duration < time.Second || c.Duration > 60*time.Second {
		return c, errors.New("require rooms=1–25 and duration=1s–60s")
	}
	if c.InferenceFloor != 0 && c.InferenceFloor != 1000 {
		return c, errors.New("inference-floor must be 0 or 1000")
	}
	if c.SetupPause < 0 || c.SetupPause > 100*time.Millisecond {
		return c, errors.New("setup-pause must be between 0 and 100ms")
	}
	if c.Scenario != "mixed" && c.Scenario != "max-enemies" {
		return c, errors.New("scenario must be mixed or max-enemies")
	}
	if c.Profile != "mixed" && c.Profile != "trainee" && c.Profile != "expert" && c.Profile != "ace" {
		return c, errors.New("profile must be mixed, trainee, expert, or ace")
	}
	for i, p := range c.Paths {
		if p == "" {
			return c, fmt.Errorf("-%s is required", profileNames[i])
		}
	}
	return c, nil
}

type countedPolicy struct {
	model                       *policymlp.Model
	calls, warmupCalls, invalid uint64
	warming                     bool
	firstError                  string
	observations                *observationCorpus
}

func (p *countedPolicy) Name() string { return p.model.Name() }
func (p *countedPolicy) Decide(o game.Observation, m *game.PolicyMemory) game.Action {
	p.calls++
	if p.warming {
		p.warmupCalls++
	}
	_, a, err := p.model.Evaluate(o, m)
	if err != nil {
		p.invalid++
		if p.firstError == "" {
			p.firstError = err.Error()
		}
		return game.Action{}
	}
	if p.observations != nil {
		p.observations.capture(o)
	}
	return a
}
func loadPolicies(paths [3]string, allowTestFixtures bool) ([3]*countedPolicy, error) {
	var policies [3]*countedPolicy
	for i, path := range paths {
		model, err := policymlp.Load(path)
		if err != nil {
			return policies, fmt.Errorf("%s: %w", profileNames[i], err)
		}
		meta := model.Metadata()
		if meta.Difficulty != profileNames[i] {
			return policies, fmt.Errorf("-%s artifact declares difficulty %s", profileNames[i], meta.Difficulty)
		}
		if !meta.Trained && !allowTestFixtures {
			return policies, fmt.Errorf("-%s artifact is explicitly untrained", profileNames[i])
		}
		policies[i] = &countedPolicy{model: model}
	}
	return policies, nil
}

type room struct {
	game                              *game.Game
	state                             game.State
	bot                               patrolDefender
	profile, level, index, generation int
	inputSequence                     uint64
}

type modelReport struct {
	Metadata                     policymlp.Metadata `json:"metadata"`
	Decisions                    uint64             `json:"decisions_including_replacement_preroll"`
	LiveDecisions                uint64             `json:"live_decisions"`
	ReplacementPrerollDecisions  uint64             `json:"replacement_preroll_decisions"`
	InvalidDecisions             uint64             `json:"invalid_decisions"`
	FirstError                   string             `json:"first_error,omitempty"`
	SupplementalDecisions        uint64             `json:"supplemental_decisions"`
	InvalidSupplementalDecisions uint64             `json:"invalid_supplemental_decisions"`
	FirstSupplementalError       string             `json:"first_supplemental_error,omitempty"`
}

type report struct {
	Scope                           string         `json:"scope"`
	Limitations                     []string       `json:"limitations"`
	OS                              string         `json:"os"`
	Arch                            string         `json:"arch"`
	GoVersion                       string         `json:"go_version"`
	GOMAXPROCS                      int            `json:"gomaxprocs"`
	GoMemoryLimitBytes              int64          `json:"go_memory_limit_bytes"`
	Scenario                        string         `json:"scenario"`
	Profile                         string         `json:"profile"`
	Rooms                           int            `json:"rooms"`
	RequestedSeconds                float64        `json:"requested_seconds"`
	ElapsedSeconds                  float64        `json:"elapsed_seconds"`
	SetupSeconds                    float64        `json:"setup_seconds"`
	SetupCPUSeconds                 float64        `json:"setup_cpu_seconds"`
	SetupPauseMS                    float64        `json:"setup_pause_ms"`
	CPUSeconds                      float64        `json:"cpu_seconds"`
	MeanCPUCores                    float64        `json:"mean_cpu_cores"`
	PeakRSSBytes                    int64          `json:"peak_rss_bytes"`
	HeapAllocBytes                  uint64         `json:"final_go_heap_alloc_bytes"`
	AllocatedBytes                  uint64         `json:"allocated_bytes"`
	Allocations                     uint64         `json:"allocations"`
	GCs                             uint32         `json:"gc_cycles"`
	TickBatches                     int            `json:"tick_batches"`
	RoomPhysicsTicks                int            `json:"room_physics_ticks"`
	MeanPhysicsHzPerRoom            float64        `json:"mean_physics_hz_per_room"`
	DroppedTickBatches              int            `json:"dropped_tick_batches"`
	CatchupTickBatches              int            `json:"catchup_tick_batches"`
	MaxTicksBehind                  int            `json:"max_ticks_behind"`
	WakeLatenessP99MS               float64        `json:"wake_lateness_p99_ms"`
	MaxWakeLatenessMS               float64        `json:"max_wake_lateness_ms"`
	WorkBatchP99MS                  float64        `json:"work_batch_p99_ms"`
	MaxWorkBatchMS                  float64        `json:"max_work_batch_ms"`
	Snapshots                       uint64         `json:"serialized_snapshots"`
	SnapshotPayloadBytes            uint64         `json:"snapshot_payload_bytes"`
	Inputs                          uint64         `json:"input_messages"`
	InputPayloadBytes               uint64         `json:"input_payload_bytes"`
	SnapshotsPerRoomSecond          float64        `json:"snapshots_per_room_second"`
	PeakEnemies                     int            `json:"peak_live_enemies"`
	MeanEnemiesPerRoom              float64        `json:"mean_enemies_per_room_at_tick_samples"`
	FullEnemyTickFraction           float64        `json:"fraction_ticks_with_four_enemies_in_every_room"`
	LivePolicyDecisions             uint64         `json:"live_policy_decisions"`
	LiveDecisionsPerSecond          float64        `json:"live_decisions_per_second"`
	InferenceFloor                  int            `json:"inference_floor"`
	SupplementalPolicyDecisions     uint64         `json:"supplemental_policy_decisions"`
	SupplementalDecisionsPerSecond  float64        `json:"supplemental_decisions_per_second"`
	CombinedPolicyDecisions         uint64         `json:"combined_policy_decisions"`
	CombinedDecisionsPerSecond      float64        `json:"combined_decisions_per_second"`
	InferenceFloorTarget            uint64         `json:"inference_floor_scheduled_target_calls"`
	SupplementalObservationCapacity int            `json:"supplemental_observation_capacity"`
	ValidObservationsCaptured       uint64         `json:"valid_observations_captured_including_setup"`
	ValidObservationsUsed           uint64         `json:"valid_observations_used_for_supplemental_inference"`
	SupplementalObservationSource   string         `json:"supplemental_observation_source,omitempty"`
	RoomRestarts                    int            `json:"room_restarts"`
	CompletedWins                   int            `json:"completed_defender_wins"`
	CompletedLosses                 int            `json:"completed_defender_losses"`
	CompletedKills                  int            `json:"kills_in_completed_rooms"`
	LevelStarts                     map[int]int    `json:"level_starts"`
	ProfileStarts                   map[string]int `json:"profile_starts"`
	Models                          []modelReport  `json:"models"`
	Cgroup                          map[string]any `json:"cgroup,omitempty"`
	Interrupted                     bool           `json:"interrupted"`
}

type workload struct {
	config       configuration
	policies     [3]*countedPolicy
	rooms        []room
	report       report
	enemySamples uint64
	fullSamples  int
	fill         *inferenceFill
}

func newWorkload(ctx context.Context, c configuration, p [3]*countedPolicy) (*workload, error) {
	w := &workload{config: c, policies: p, rooms: make([]room, c.Rooms), report: report{LevelStarts: map[int]int{}, ProfileStarts: map[string]int{}}}
	if c.InferenceFloor != 0 {
		w.fill = &inferenceFill{}
	}
	for _, policy := range p {
		policy.observations = nil
		if w.fill != nil {
			policy.observations = &w.fill.corpus
		}
	}
	for i := range w.rooms {
		// This optional pacing belongs only to initial setup. Keeping it outside
		// resetRoom prevents pauses during measured replacement pre-roll.
		if err := pauseInitialSetup(ctx, c.SetupPause); err != nil {
			return nil, err
		}
		w.rooms[i].index = i
		if err := w.resetRoom(ctx, &w.rooms[i]); err != nil {
			return nil, err
		}
	}
	if w.fill != nil {
		if err := w.prepareInferenceFloor(ctx); err != nil {
			return nil, err
		}
		w.fill.liveBaseline = liveCalls(p)
	}
	return w, nil
}

func pauseInitialSetup(ctx context.Context, pause time.Duration) error {
	if pause == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (w *workload) resetRoom(ctx context.Context, r *room) error {
	generation := r.generation
	r.profile = (r.index + generation) % 3
	if w.config.Profile != "mixed" {
		for i, name := range profileNames {
			if name == w.config.Profile {
				r.profile = i
			}
		}
	}
	r.level = 1 + (r.index+generation)%10
	if w.config.Scenario == "max-enemies" {
		r.level = 7 + (r.index+generation)%4
	}
	level, _ := levels.Get(r.level)
	p := w.policies[r.profile]
	p.warming = true
	defer func() { p.warming = false }()
	ready := false
	maxSeen := 0
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		seed := w.config.Seed + int64(r.index)*1000003 + int64(generation)*7919 + int64(attempt)*37
		r.game = game.New(level, seed)
		r.game.SetPolicy(p)
		r.inputSequence = 0
		r.bot = patrolDefender{phase: r.index + generation}
		if w.config.Scenario == "mixed" {
			ready = true
			break
		}
		// Legal idle pre-roll creates four live tanks through the real spawn
		// pipeline. No private game state is set, and no map/wave is altered.
		fullFor := 0
		for tick := 0; tick < 360; tick++ {
			if tick%30 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			r.game.Step(1.0 / game.SimulationHz)
			s := r.game.Snapshot()
			maxSeen = max(maxSeen, len(s.Enemies))
			if s.Status != "playing" {
				break
			}
			if len(s.Enemies) == 4 {
				fullFor++
			} else {
				fullFor = 0
			}
			if fullFor >= 9 {
				ready = true
				break
			} // Include the real two-sample warmup.
		}
		if ready {
			break
		}
	}
	if !ready {
		return fmt.Errorf("could not legally prefill four enemies for %s level %d within the bounded setup budget (maximum seen %d)", profileNames[r.profile], r.level, maxSeen)
	}
	r.state = r.game.Snapshot()
	r.generation++
	w.report.LevelStarts[r.level]++
	w.report.ProfileStarts[profileNames[r.profile]]++
	return nil
}

type inputMessage struct {
	Type string `json:"type"`
	Seq  uint64 `json:"seq"`
	Move int    `json:"move"`
	Fire bool   `json:"fire"`
}

func (w *workload) tick(ctx context.Context, index int) error {
	live := 0
	for i := range w.rooms {
		r := &w.rooms[i]
		if index%2 == 0 {
			a := r.bot.action(r.state)
			r.inputSequence++
			in := game.Input{Seq: r.inputSequence, Move: a.Move, Fire: a.Fire}
			if w.config.Serialize {
				payload, err := json.Marshal(inputMessage{Type: "input", Seq: in.Seq, Move: in.Move, Fire: in.Fire})
				if err != nil {
					return err
				}
				var decoded inputMessage
				if err = json.Unmarshal(payload, &decoded); err != nil {
					return err
				}
				in = game.Input{Seq: decoded.Seq, Move: decoded.Move, Fire: decoded.Fire}
				w.report.InputPayloadBytes += uint64(len(payload))
			}
			r.game.SetInput(in)
			w.report.Inputs++
		}
		r.game.Step(1.0 / game.SimulationHz)
		w.report.RoomPhysicsTicks++
		if r.game.Status() != "playing" {
			final := r.game.Snapshot()
			w.report.CompletedKills += final.Kills
			if final.Status == "won" {
				w.report.CompletedWins++
			} else {
				w.report.CompletedLosses++
			}
			if err := w.resetRoom(ctx, r); err != nil {
				return err
			}
			w.report.RoomRestarts++
		}
		if index%2 == 0 || w.config.Scenario == "max-enemies" {
			r.state = r.game.Snapshot()
		}
		live += len(r.state.Enemies)
		if index%2 == 0 && w.config.Serialize {
			payload, err := json.Marshal(struct {
				Type  string     `json:"type"`
				State game.State `json:"state"`
			}{"state", r.state})
			if err != nil {
				return err
			}
			w.report.Snapshots++
			w.report.SnapshotPayloadBytes += uint64(len(payload))
		}
	}
	w.report.PeakEnemies = max(w.report.PeakEnemies, live)
	w.enemySamples += uint64(live)
	if live == 4*len(w.rooms) {
		w.fullSamples++
	}
	if w.fill != nil {
		return w.fillInferenceFloor(ctx)
	}
	return nil
}

func measure(ctx context.Context, c configuration, p [3]*countedPolicy) (report, error) {
	setupStart := time.Now()
	setupCPU, err := processUsage()
	if err != nil {
		return report{}, err
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 15*time.Second)
	w, err := newWorkload(setupCtx, c, p)
	setupCancel()
	if err != nil {
		return report{}, err
	}
	setupEnd, err := processUsage()
	if err != nil {
		return report{}, err
	}
	result := w.report
	result.Scope = "Standalone real-game simulation + learned actors + optional JSON; no network listener, HTTP, leaderboard or deployment"
	result.Limitations = []string{
		"CPU includes cheap scripted defender and optional client input marshaling; these normally run on the client.",
		"Excludes HTTP/WebSocket/TLS, network goroutines, kernel socket buffers, leaderboard and application process overhead; not full-server quota certification.",
		"max-enemies starts each room/replacement at four live enemies; deaths and normal spawn cadence may lower occupancy. Actual occupancy/inference counts are reported.",
		"Initial pre-roll is separately reported; replacement pre-roll is included in measured CPU and wall time. RSS peak covers the process lifetime.",
		"GOMEMLIMIT is a soft Go-runtime target, not a hard process/container memory limit. Cgroup counters may include other processes in the same group.",
		"An optional inference floor adds MLP-only calls on captured real delayed observations, with separate policy memory and discarded actions. Combined inference is not a count of live enemies; actor observation construction and tank physics remain at actual occupancy.",
	}
	result.OS, result.Arch, result.GoVersion = runtime.GOOS, runtime.GOARCH, runtime.Version()
	result.GOMAXPROCS = runtime.GOMAXPROCS(0)
	result.GoMemoryLimitBytes = debug.SetMemoryLimit(-1)
	result.Scenario, result.Profile, result.Rooms = c.Scenario, c.Profile, c.Rooms
	result.InferenceFloor = c.InferenceFloor
	result.RequestedSeconds = c.Duration.Seconds()
	result.SetupSeconds = time.Since(setupStart).Seconds()
	result.SetupCPUSeconds = setupEnd.CPUSeconds - setupCPU.CPUSeconds
	result.SetupPauseMS = float64(c.SetupPause) / float64(time.Millisecond)
	var callsBefore, warmBefore, errorsBefore [3]uint64
	for i, p := range p {
		callsBefore[i], warmBefore[i], errorsBefore[i] = p.calls, p.warmupCalls, p.invalid
	}
	var memoryBefore, memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	cpuBefore, err := processUsage()
	if err != nil {
		return result, err
	}
	cgBefore := readCgroup()
	cgPeak := int64Value(cgBefore, "memory_current_bytes")
	started := time.Now()
	deadline := started.Add(c.Duration)
	period := time.Second / game.SimulationHz
	next := started.Add(period)
	measureCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	workSamples := make([]float64, 0, 1801)
	wakeSamples := make([]float64, 0, 1801)
	batches, dropped, catchup, maxBehind := 0, 0, 0, 0
	var runErr error
loop:
	for next.Before(deadline) || next.Equal(deadline) {
		if err := measureCtx.Err(); err != nil {
			break
		}
		wait := time.Until(next)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-measureCtx.Done():
				timer.Stop()
				break loop
			}
		}
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		late := max(time.Duration(0), now.Sub(next))
		behind := int(late / period)
		due := 1 + behind
		maxBehind = max(maxBehind, behind)
		wakeSamples = append(wakeSamples, float64(late)/float64(time.Millisecond))
		count := min(6, due)
		dropped += due - count
		next = next.Add(time.Duration(due) * period)
		for step := 0; step < count; step++ {
			if measureCtx.Err() != nil {
				break loop
			}
			workStart := time.Now()
			if err := w.tick(measureCtx, batches); err != nil {
				if measureCtx.Err() == nil {
					runErr = err
				}
				break loop
			}
			workSamples = append(workSamples, float64(time.Since(workStart))/float64(time.Millisecond))
			if step > 0 {
				catchup++
			}
			batches++
		}
		if batches%30 == 0 {
			cgPeak = max(cgPeak, int64Value(readCgroup(), "memory_current_bytes"))
		}
	}
	// Wait out a fractional final interval so requested seconds describe a paced
	// measurement rather than an early return after the last scheduled tick.
	if runErr == nil && time.Now().Before(deadline) {
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	elapsed := time.Since(started).Seconds()
	cpuAfter, err := processUsage()
	if err != nil {
		return result, err
	}
	runtime.ReadMemStats(&memoryAfter)
	cgAfter := readCgroup()
	cgPeak = max(cgPeak, int64Value(cgAfter, "memory_current_bytes"))
	// Copy counters accumulated by the workload, preserving setup/identity data.
	counter := w.report
	result.ElapsedSeconds = elapsed
	result.CPUSeconds = cpuAfter.CPUSeconds - cpuBefore.CPUSeconds
	result.MeanCPUCores = result.CPUSeconds / elapsed
	result.PeakRSSBytes = cpuAfter.PeakRSSBytes
	result.HeapAllocBytes = memoryAfter.HeapAlloc
	result.AllocatedBytes = memoryAfter.TotalAlloc - memoryBefore.TotalAlloc
	result.Allocations = memoryAfter.Mallocs - memoryBefore.Mallocs
	result.GCs = memoryAfter.NumGC - memoryBefore.NumGC
	result.TickBatches = batches
	result.RoomPhysicsTicks = counter.RoomPhysicsTicks
	result.MeanPhysicsHzPerRoom = float64(counter.RoomPhysicsTicks) / float64(c.Rooms) / elapsed
	result.DroppedTickBatches = dropped
	result.CatchupTickBatches = catchup
	result.MaxTicksBehind = maxBehind
	result.WakeLatenessP99MS, result.MaxWakeLatenessMS = percentileAndMax(wakeSamples, .99)
	result.WorkBatchP99MS, result.MaxWorkBatchMS = percentileAndMax(workSamples, .99)
	result.Snapshots, result.SnapshotPayloadBytes = counter.Snapshots, counter.SnapshotPayloadBytes
	result.Inputs, result.InputPayloadBytes = counter.Inputs, counter.InputPayloadBytes
	result.SnapshotsPerRoomSecond = float64(counter.Snapshots) / float64(c.Rooms) / elapsed
	result.PeakEnemies = counter.PeakEnemies
	result.MeanEnemiesPerRoom = float64(w.enemySamples) / float64(max(1, batches)*c.Rooms)
	result.FullEnemyTickFraction = float64(w.fullSamples) / float64(max(1, batches))
	result.RoomRestarts, result.CompletedWins, result.CompletedLosses, result.CompletedKills = counter.RoomRestarts, counter.CompletedWins, counter.CompletedLosses, counter.CompletedKills
	result.LevelStarts, result.ProfileStarts = counter.LevelStarts, counter.ProfileStarts
	for i, p := range p {
		calls, warm := p.calls-callsBefore[i], p.warmupCalls-warmBefore[i]
		model := modelReport{Metadata: p.model.Metadata(), Decisions: calls, LiveDecisions: calls - warm, ReplacementPrerollDecisions: warm, InvalidDecisions: p.invalid - errorsBefore[i], FirstError: p.firstError}
		if w.fill != nil {
			model.SupplementalDecisions = w.fill.calls[i]
			model.InvalidSupplementalDecisions = w.fill.invalid[i]
			model.FirstSupplementalError = w.fill.firstError[i]
			result.SupplementalPolicyDecisions += w.fill.calls[i]
		}
		result.Models = append(result.Models, model)
		result.LivePolicyDecisions += calls - warm
		if p.invalid > 0 && runErr == nil {
			runErr = fmt.Errorf("%s actor rejected a real observation: %s", profileNames[i], p.firstError)
		}
	}
	result.LiveDecisionsPerSecond = float64(result.LivePolicyDecisions) / elapsed
	result.SupplementalDecisionsPerSecond = float64(result.SupplementalPolicyDecisions) / elapsed
	result.CombinedPolicyDecisions = result.LivePolicyDecisions + result.SupplementalPolicyDecisions
	result.CombinedDecisionsPerSecond = float64(result.CombinedPolicyDecisions) / elapsed
	if w.fill != nil {
		result.InferenceFloorTarget = w.fill.target
		result.SupplementalObservationCapacity = observationCorpusCapacity
		result.ValidObservationsCaptured = w.fill.corpus.captured
		result.ValidObservationsUsed = w.fill.corpus.used
		result.SupplementalObservationSource = "Validated real delayed policy observations from legal setup/warmup and live Game.Step calls; fixed ring refreshes on actual actor calls. Used counts capture instances, not distinct numeric vectors."
	}
	result.Interrupted = ctx.Err() != nil
	result.Cgroup = cgroupDelta(cgBefore, cgAfter, cgPeak)
	return result, runErr
}
func percentileAndMax(values []float64, q float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	index := int(float64(len(copyValues)-1) * q)
	return copyValues[index], copyValues[len(copyValues)-1]
}

func main() {
	c, err := parseConfiguration(os.Args[1:], os.Stderr)
	if err == nil {
		var policies [3]*countedPolicy
		policies, err = loadPolicies(c.Paths, false)
		if err == nil {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			var result report
			result, err = measure(ctx, c, policies)
			if result.Rooms > 0 {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				if writeErr := encoder.Encode(result); writeErr != nil && err == nil {
					err = writeErr
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "budgetcheck:", err)
		os.Exit(1)
	}
}
