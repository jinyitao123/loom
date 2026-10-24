package stdlib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// Paused controlled loops compare this identity on resume. Loops that do not
// opt into a tool choice policy must keep their pre-existing identity bytes.
func TestToolLoopPolicyHashUnchangedWithoutToolChoicePolicy(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	base := ToolLoopOpts{Model: "m", SystemPrompt: "s", MaxIterations: 4, MaxToolRepeats: 3, MaxTokens: 9,
		Effort: contract.EffortMedium, OutputSchema: &schema, Control: &ToolLoopControl{ID: "loop", InitialTotalRounds: 8}}
	digest := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	v1 := digest(struct {
		Version                     uint32
		Control                     ToolLoopControl
		Model, System               string
		Iterations, Repeats, Tokens int
		Effort                      contract.EffortLevel
		Schema                      *json.RawMessage
	}{1, *base.Control, "m", "s", 4, 3, 9, contract.EffortMedium, &schema})
	if got, err := toolLoopPolicyHash(base, loom.State{}); err != nil || got != v1 {
		t.Fatalf("plain identity = %s, %v; want %s", got, err, v1)
	}
	verified := base
	verified.CompletionVerifier = CompletionVerifierFunc(func(context.Context, CompletionCandidate) (CompletionDecision, error) {
		return CompletionDecision{Accepted: true}, nil
	})
	verified.CompletionVerifierID = "fixture-v1"
	v2 := digest(struct {
		Version                     uint32
		Control                     ToolLoopControl
		Model, System               string
		Iterations, Repeats, Tokens int
		Effort                      contract.EffortLevel
		Schema                      *json.RawMessage
		CompletionVerifierID        string
	}{2, *base.Control, "m", "s", 4, 3, 9, contract.EffortMedium, &schema, "fixture-v1"})
	if got, err := toolLoopPolicyHash(verified, loom.State{}); err != nil || got != v2 {
		t.Fatalf("verified identity = %s, %v; want %s", got, err, v2)
	}
	chosen := verified
	chosen.ToolChoicePolicy = ToolChoicePolicyFunc(func(context.Context, ToolChoiceInput) (*contract.ToolChoice, error) { return nil, nil })
	chosen.ToolChoicePolicyID = "choice-v1"
	got, err := toolLoopPolicyHash(chosen, loom.State{})
	if err != nil || got == v2 || got == v1 {
		t.Fatalf("tool choice identity must differ: %s, %v", got, err)
	}
	changed := chosen
	changed.ToolChoicePolicyID = "choice-v2"
	if other, _ := toolLoopPolicyHash(changed, loom.State{}); other == got {
		t.Fatal("tool choice policy id is not part of the identity")
	}
}

func TestProviderDefaultEffortHashesApartAndKeepsExistingHashes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	base := ToolLoopOpts{Model: "m", SystemPrompt: "s", MaxIterations: 4, MaxToolRepeats: 3, MaxTokens: 9,
		OutputSchema: &schema, Control: &ToolLoopControl{ID: "loop", InitialTotalRounds: 8}}
	hash := func(opts ToolLoopOpts) string {
		t.Helper()
		value, err := toolLoopPolicyHash(opts, loom.State{})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	filled := hash(base)
	providerDefault := base
	providerDefault.ProviderDefaultEffort = true
	if hash(providerDefault) == filled {
		t.Fatal("an omitted effort hashed like one filled in as medium")
	}
	explicit := base
	explicit.Effort = contract.EffortHigh
	withFlag := explicit
	withFlag.ProviderDefaultEffort = true
	if hash(withFlag) != hash(explicit) {
		t.Fatal("ProviderDefaultEffort changed the hash of an explicit effort")
	}
}

// Sampling and thinking settings are part of a controlled loop's identity once
// set, and leave the identity of loops without them unchanged.
func TestSamplingAndThinkingHashApartOnlyWhenSet(t *testing.T) {
	base := ToolLoopOpts{Model: "m", SystemPrompt: "s", MaxIterations: 4, Effort: contract.EffortLow,
		Control: &ToolLoopControl{ID: "loop", InitialTotalRounds: 8}}
	hash := func(opts ToolLoopOpts) string {
		t.Helper()
		value, err := toolLoopPolicyHash(opts, loom.State{})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	legacy, err := toolLoopBasePolicyHash(base, loom.State{})
	if err != nil || hash(base) != legacy {
		t.Fatalf("a loop without sampling settings changed identity: %v", err)
	}
	zero, half := 0.0, 0.5
	seen := map[string]string{"none": hash(base)}
	for name, change := range map[string]func(*ToolLoopOpts){
		"temperature 0":   func(o *ToolLoopOpts) { o.Temperature = &zero },
		"temperature 0.5": func(o *ToolLoopOpts) { o.Temperature = &half },
		"top_p 0.5":       func(o *ToolLoopOpts) { o.TopP = &half },
		"thinking":        func(o *ToolLoopOpts) { o.Thinking = contract.ThinkingDisabled },
	} {
		opts := base
		change(&opts)
		value := hash(opts)
		for other, otherValue := range seen {
			if otherValue == value {
				t.Fatalf("%s hashed like %s", name, other)
			}
		}
		seen[name] = value
	}
}
