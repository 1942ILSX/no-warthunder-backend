package difficulty

import (
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/policymlp"
	"fmt"
	"path/filepath"
)

// The qualification belongs to both the weights and the sampler. Pin the
// approved release so swapping in a parity fixture, an older candidate, or an
// argmax sampler cannot silently advertise a qualified difficulty. A future
// release updates these pins after evaluation, not from a client request.
var qualified = map[ID]struct {
	version     string
	weightsHash string
	temperature float64
}{
	Trainee: {"nwt-trainee-v3", "fa76ca60fe3b571e75adeffeb45ed3035f691d8d0ed87641629bf2776b77e7e8", 1},
	Expert:  {"nwt-expert-v3", "da38ee126a5fbfadd40f22a08931d440cce17f05a85ddb00f72193889521fec8", .3},
	Ace:     {"nwt-ace-v3", "c1c1a63b158676173ab3125f0f1c9eac249942b93e0caed1853f48fda93ef0f9", .5},
}

// LoadPolicies validates all three artifacts before any HTTP listener starts.
// One immutable MLP per difficulty is shared across rooms; per-tank RNG and
// action history stay in game.PolicyMemory. There is no Python runtime or RPC.
func LoadPolicies(root string) (map[ID]game.Policy, error) {
	policies := make(map[ID]game.Policy, 3)
	for _, profile := range All() {
		model, err := policymlp.Load(filepath.Join(root, string(profile.ID)))
		if err != nil {
			return nil, fmt.Errorf("%s model: %w", profile.ID, err)
		}
		metadata := model.Metadata()
		expected := qualified[profile.ID]
		if !metadata.Trained || metadata.Difficulty != string(profile.ID) ||
			metadata.PolicyVersion != expected.version || metadata.WeightsSHA256 != expected.weightsHash ||
			metadata.Sampler.Mode != "categorical" || metadata.Sampler.Temperature != expected.temperature ||
			metadata.Sampler.RNG != policymlp.SamplerRNG {
			return nil, fmt.Errorf("%s model: expected qualified %s weights and categorical sampler at temperature %g", profile.ID, expected.version, expected.temperature)
		}
		policies[profile.ID] = model
	}
	return policies, nil
}
