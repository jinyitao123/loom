package requestcheck

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// Native adapters cannot send per-request thinking or top_p, so a request
// carrying either is refused instead of losing it on the way.
func TestValidateRefusesWhatNativeAdaptersCannotSend(t *testing.T) {
	topP := 0.5
	for name, req := range map[string]contract.ChatRequest{
		"thinking": {Thinking: contract.ThinkingEnabled},
		"top_p":    {TopP: &topP},
	} {
		if err := Validate(req); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if err := Validate(contract.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
}
