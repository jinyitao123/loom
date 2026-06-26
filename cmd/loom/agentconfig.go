package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/jinyitao123/loom/stdlib"
)

// loadAgentSpec reads --agent-config: a serialized stdlib.AgentSpec the weave
// daemon compiles from the agent's rich config (system prompt / identity /
// skills / profiles). A missing path/file is not an error — the agent simply
// runs with no rich config.
func loadAgentSpec(path string) (*stdlib.AgentSpec, error) {
	if path == "" {
		return nil, nil
	}
	// #nosec G304 -- path is the operator/daemon-supplied .loom-agent.json in the
	// agent workdir, not untrusted remote input.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var spec stdlib.AgentSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse agent-config %s: %w", path, err)
	}
	return &spec, nil
}

// specIdentity resolves the identity used for prompt assembly, falling back to
// the flat SystemPrompt (v1.2 compat) when no tiered identity is set.
func specIdentity(s *stdlib.AgentSpec) stdlib.IdentitySpec {
	id := s.Identity
	if id.Core == "" && id.Extended == "" && id.Raw == "" && s.SystemPrompt != "" {
		id.Core = s.SystemPrompt
	}
	return id
}
