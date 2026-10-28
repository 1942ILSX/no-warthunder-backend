// Package game contains the deterministic, rendering-free Battle City simulation.
// A room owns one Game; callers must serialize Step, SetInput, and Snapshot.
// Keep network, filesystem, wall-clock time, and training code out of this package.
package game

import "BattleCityReforge/internal/levels"

const (
	BoardSize            = 24
	SimulationHz         = 30
	DecisionHz           = 10
	TankRadius           = 0.36
	PlayerSpeed          = 3.15
	PlayerBulletSpeed    = 10.0
	EnemyBulletSpeed     = 5.8
	MaxSimulationSeconds = 15 * 60
)

// Directions are clockwise, beginning at north. Input Move is direction + 1;
// Move=0 keeps the previous facing and stops translation. Fire is independent.
type Input struct {
	Seq  uint64 `json:"seq"`
	Move int    `json:"move"`
	Fire bool   `json:"fire"`
}

type Tank struct {
	ID     int     `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Dir    int     `json:"dir"`
	HP     int     `json:"hp"`
	MaxHP  int     `json:"maxHp"`
	Kind   string  `json:"kind"`
	Shield bool    `json:"shield"`
}

type Bullet struct {
	ID    int     `json:"id"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Dir   int     `json:"dir"`
	Owner string  `json:"owner"`
}

type Effect struct {
	ID   int     `json:"id"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Kind string  `json:"kind"`
	TTL  float64 `json:"ttl"`
}

type Base struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	HP    int     `json:"hp"`
	MaxHP int     `json:"maxHp"`
}

type State struct {
	Tick             uint64   `json:"tick"`
	TimeMS           int64    `json:"timeMs"`
	Status           string   `json:"status"`
	Grid             []string `json:"grid"`
	Player           Tank     `json:"player"`
	Enemies          []Tank   `json:"enemies"`
	Bullets          []Bullet `json:"bullets"`
	Effects          []Effect `json:"effects"`
	Base             Base     `json:"base"`
	Score            int      `json:"score"`
	Kills            int      `json:"kills"`
	TotalEnemies     int      `json:"totalEnemies"`
	EnemiesRemaining int      `json:"enemiesRemaining"`
	Lives            int      `json:"lives"`
	LastInputSeq     uint64   `json:"lastInputSeq"`
}

type tankState struct {
	Tank
	cooldown        float64
	shieldTime      float64
	action          Action
	memory          PolicyMemory
	perception      [2]Observation // Two decisions of perception delay, as in training.
	perceptionCount int
	previousX       float64
	previousY       float64
	stuckTime       float64
	speed           float64
	shotInterval    float64
	role            int
}

type projectile struct {
	Bullet
	speed    float64
	sourceID int
	dead     bool
}

// Game has no goroutines. One server-owned loop updates all state, eliminating
// synchronization and per-tank runtime overhead. A model's weights can be shared
// between many Games through SetPolicy; transient policy memory stays per tank.
type Game struct {
	level          levels.Level
	grid           [BoardSize][BoardSize]byte
	gridRows       [BoardSize]string
	gridDirty      bool
	player         tankState
	enemies        []tankState
	bullets        []projectile
	effects        []Effect
	base           Base
	input          Input
	tick           uint64
	elapsed        float64
	status         string
	lives          int
	score          int
	kills          int
	spawned        int
	spawnTimer     float64
	respawnTimer   float64
	decisionTimer  float64
	nextID         int
	random         uint64
	policy         Policy
	playerVelocity [2]float64
	navPlayer      [BoardSize][BoardSize]float32
	navBase        [BoardSize][BoardSize]float32
	navPlayerCell  int
	navDirty       bool
	decisionHook   DecisionHook // nil in production; no training channels in Game
	decisionSlots  [DecisionSlots]int
}

var directionX = [4]float64{0, 1, 0, -1}
var directionY = [4]float64{-1, 0, 1, 0}
