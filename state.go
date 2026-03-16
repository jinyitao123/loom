package loom

import "encoding/json"

// State is the execution context shared across all steps.
// Keys are strings. Values are any JSON-serializable type.
type State map[string]any

func (s State) Marshal() ([]byte, error)     { return json.Marshal(s) }
func UnmarshalState(b []byte) (State, error) {
	var s State
	err := json.Unmarshal(b, &s)
	return s, err
}

// MergePolicy defines how a specific key is merged.
type MergePolicy func(existing, incoming any) any

// Built-in policies.
var (
	Overwrite MergePolicy = func(_, incoming any) any { return incoming }

	AppendSlice MergePolicy = func(existing, incoming any) any {
		e, _ := existing.([]any)
		i, _ := incoming.([]any)
		return append(e, i...)
	}

	SumInt MergePolicy = func(existing, incoming any) any {
		e, _ := existing.(int)
		i, _ := incoming.(int)
		return e + i
	}

	SumFloat MergePolicy = func(existing, incoming any) any {
		e, _ := existing.(float64)
		i, _ := incoming.(float64)
		return e + i
	}
)

// MergeConfig holds per-key merge policies.
// It is mutable during construction and frozen once passed to a Graph.
type MergeConfig struct {
	policies map[string]MergePolicy
	fallback MergePolicy
	frozen   bool
}

func NewMergeConfig() *MergeConfig {
	return &MergeConfig{
		policies: make(map[string]MergePolicy),
		fallback: Overwrite,
	}
}

func (mc *MergeConfig) Register(key string, policy MergePolicy) {
	if mc.frozen {
		panic("loom: cannot Register on a frozen MergeConfig (already attached to a Graph)")
	}
	mc.policies[key] = policy
}

// DefaultMergeConfig returns stdlib's recommended defaults.
func DefaultMergeConfig() *MergeConfig {
	mc := NewMergeConfig()
	mc.Register("messages", AppendSlice)
	return mc
}

// Merge applies the update to the current state using registered policies.
func (s State) Merge(update State, cfg *MergeConfig) State {
	if cfg == nil {
		cfg = &MergeConfig{fallback: Overwrite}
	}
	result := make(State, len(s)+len(update))
	for k, v := range s {
		result[k] = v
	}
	for k, v := range update {
		if policy, ok := cfg.policies[k]; ok {
			result[k] = policy(result[k], v)
		} else {
			result[k] = cfg.fallback(result[k], v)
		}
	}
	return result
}
