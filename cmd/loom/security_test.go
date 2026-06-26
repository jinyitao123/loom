package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The session store must never let a crafted ns/key escape its root directory.
// url.PathEscape turns "../" into "..%2F" — a single, contained path segment.
func TestFileStorePathTraversalContained(t *testing.T) {
	root := t.TempDir()
	s := newFileStore(root)
	if err := s.Put(context.Background(), "../../etc", "passwd", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if p := s.path("../../etc", "passwd"); !strings.HasPrefix(p, root) {
		t.Fatalf("path escaped the store root: %s (root %s)", p, root)
	}
	if p := s.path("ns", "../../../../etc/shadow"); !strings.HasPrefix(p, root) {
		t.Fatalf("key escaped the store root: %s (root %s)", p, root)
	}
}

// The provider API key must never appear in the stdout-json event stream — not
// even on the error path (where upstream errors get surfaced).
func TestSecretsNotLeakedToStdout(t *testing.T) {
	const sentinel = "sk-SECRET-SENTINEL-DO-NOT-LEAK"
	srv := httptest.NewServer((&mockLLM{turns: []llmTurn{{httpError: 500}}}).handler())
	defer srv.Close()

	var out bytes.Buffer
	runCmdIO(
		[]string{"--model", "test", "--cwd", t.TempDir()},
		strings.NewReader(`{"type":"chat","instruction":"hi"}`),
		&out,
		envGetter(map[string]string{"LOOM_BASE_URL": srv.URL, "LOOM_API_KEY": sentinel}),
	)
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("API key leaked into the output stream:\n%s", out.String())
	}
}
