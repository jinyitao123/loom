package main

import (
	"encoding/json"
	"io"
	"testing"
)

// The emitter is the hottest path: the daemon streams one event per token, so
// emit throughput bounds interactive latency. Track it to catch regressions.
func BenchmarkEmitterText(b *testing.B) {
	e := NewEmitter(io.Discard)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = e.Text("a streamed token delta of moderate length")
	}
}

func BenchmarkEmitterFullTurn(b *testing.B) {
	e := NewEmitter(io.Discard)
	input := json.RawMessage(`{"to":"a@b.c","subject":"hi"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = e.SessionInit("s1")
		_ = e.Text("let me check the inventory")
		_ = e.ToolUse("c1", "list_inventory", input)
		_ = e.ToolResult("c1", "3 items in stock", "success")
		_ = e.Result("completed", "found 3 items", "s1")
	}
}

func BenchmarkParsePrompt(b *testing.B) {
	raw := []byte(`{"type":"chat","instruction":"reconcile the order","notice":"use the email tool","recalled_memory":["customer prefers brevity","ticket 42 was urgent"]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parsePrompt(raw); err != nil {
			b.Fatal(err)
		}
	}
}
