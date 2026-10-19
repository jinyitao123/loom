package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type catalogTransport struct {
	s      *session
	broken bool
	paged  bool
}

func (t *catalogTransport) close() error { return nil }
func (t *catalogTransport) send(_ context.Context, raw []byte) error {
	if t.broken {
		return errors.New("catalog unavailable")
	}
	var req rpcMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	result := `{"tools":[{"name":"read","inputSchema":{"type":"object"}}]}`
	if t.paged {
		result = `{"tools":[],"nextCursor":"repeated"}`
	}
	response, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)})
	t.s.receive(response)
	return nil
}

func TestCatalogRefreshFailsClosedAndRetries(t *testing.T) {
	s := newSession("test", nil)
	transport := &catalogTransport{s: s}
	s.transport = transport
	srv := &server{name: "test", prefix: "mcp__test__", session: s, stale: true}
	m := &Manager{servers: []*server{srv}}
	ctx := context.Background()
	if _, err := m.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	transport.broken = true
	srv.stale = true
	if _, err := m.ListTools(ctx); err == nil {
		t.Fatal("failed refresh accepted a stale catalog")
	}
	if len(m.routes) != 0 {
		t.Fatal("failed refresh retained dispatch routes")
	}
	transport.broken = false
	if defs, err := m.ListTools(ctx); err != nil || len(defs) != 1 {
		t.Fatalf("refresh did not retry: %v, %v", defs, err)
	}
}

func TestCatalogNameCollisionFailsClosed(t *testing.T) {
	m := &Manager{servers: []*server{{name: "one", prefix: "", tools: []tool{{Name: "read.one"}, {Name: "read_one"}}}}}
	if _, err := m.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collision = %v", err)
	}
	if len(m.routes) != 0 {
		t.Fatal("ambiguous routes remained usable")
	}
}

func TestCatalogPaginationMustComplete(t *testing.T) {
	s := newSession("test", nil)
	s.transport = &catalogTransport{s: s, paged: true}
	if _, err := s.listTools(context.Background()); err == nil {
		t.Fatal("incomplete tool catalog was accepted")
	}
}
