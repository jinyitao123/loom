package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyitao123/loom/stdlib"
)

func TestLoadAgentSpecEmptyPathReturnsNil(t *testing.T) {
	spec, err := loadAgentSpec("")
	if err != nil || spec != nil {
		t.Fatalf("empty path: %v %v", spec, err)
	}
}

func TestLoadAgentSpecMissingFileReturnsNil(t *testing.T) {
	spec, err := loadAgentSpec(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || spec != nil {
		t.Fatalf("missing file should be (nil,nil): %v %v", spec, err)
	}
}

func TestLoadAgentSpecParses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.json")
	body := `{"system_prompt":"You are a parts clerk.","skills":[{"name":"greet","body":"Say hi.","always_active":true}],"profiles":{"formal":{"system_addition":"Be formal."}}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := loadAgentSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.SystemPrompt != "You are a parts clerk." {
		t.Fatalf("system_prompt = %q", spec.SystemPrompt)
	}
	if len(spec.Skills) != 1 || !spec.Skills[0].AlwaysActive {
		t.Fatalf("skills not parsed: %+v", spec.Skills)
	}
	if spec.Profiles["formal"].SystemAddition != "Be formal." {
		t.Fatalf("profile not parsed: %+v", spec.Profiles)
	}
}

func TestLoadAgentSpecInvalidJSONErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := loadAgentSpec(p); err == nil {
		t.Fatal("want parse error")
	}
}

func TestSpecIdentityFallsBackToSystemPrompt(t *testing.T) {
	id := specIdentity(&stdlib.AgentSpec{SystemPrompt: "sp"})
	if id.Core != "sp" {
		t.Fatalf("fallback core = %q", id.Core)
	}
}

func TestSpecIdentityPrefersTieredIdentity(t *testing.T) {
	id := specIdentity(&stdlib.AgentSpec{SystemPrompt: "sp", Identity: stdlib.IdentitySpec{Core: "core"}})
	if id.Core != "core" {
		t.Fatalf("should prefer Identity.Core, got %q", id.Core)
	}
}
