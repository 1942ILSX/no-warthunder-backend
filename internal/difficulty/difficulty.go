// Package difficulty defines the public difficulty and scoring contract.
// Models are loaded once at startup, then shared by every room. Difficulty is
// selected when reserving a room and never changes during that run.
package difficulty

type ID string

const (
	Trainee ID = "trainee"
	Expert  ID = "expert"
	Ace     ID = "ace"
	Default    = Expert
)

type Profile struct {
	ID              ID      `json:"id"`
	ScoreMultiplier float64 `json:"scoreMultiplier"`
}

func All() []Profile {
	return []Profile{{Trainee, 1}, {Expert, 1.5}, {Ace, 2}}
}

func Parse(value string) (Profile, bool) {
	for _, profile := range All() {
		if string(profile.ID) == value {
			return profile, true
		}
	}
	return Profile{}, false
}

// ApplyScore applies the locked room's multiplier once to the raw game score.
// Integer division rounds half-points down. The caller supplies a nonnegative,
// server-generated game score; the display-only floating multiplier is unused.
func (profile Profile) ApplyScore(base int) int {
	if base <= 0 {
		return 0
	}
	switch profile.ID {
	case Trainee:
		return base
	case Expert:
		return base * 3 / 2
	case Ace:
		return base * 2
	default:
		return 0
	}
}
