package openai_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

func wireBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal captured body: %v\nbody=%s", err, body)
	}
	return m
}

// A thinking mode, top_p and temperature a request asks for go on the wire as
// asked, on Chat and Stream alike: an explicit switch overrides the client's
// default and never trims the request's sampling or rewrites its effort.
func TestRequestThinkingAndSamplingAreSentAsAsked(t *testing.T) {
	temperature, topP := 0.3, 0.9
	for _, tc := range []struct {
		name     string
		opts     []openai.Option
		thinking contract.ThinkingMode
		effort   contract.EffortLevel
		sampling bool
		want     map[string]any
		absent   []string
	}{
		{name: "enabled with sampling", thinking: contract.ThinkingEnabled, effort: contract.EffortHigh, sampling: true,
			want: map[string]any{"thinking": "enabled", "temperature": 0.3, "top_p": 0.9, "reasoning_effort": "high"}},
		{name: "disabled keeps its effort", thinking: contract.ThinkingDisabled, effort: contract.EffortLow,
			want: map[string]any{"thinking": "disabled", "reasoning_effort": "low"}, absent: []string{"temperature", "top_p"}},
		{name: "explicit over the client default", opts: []openai.Option{openai.WithThinkingControl("enabled", false)},
			thinking: contract.ThinkingDisabled, sampling: true,
			want: map[string]any{"thinking": "disabled", "temperature": 0.3, "top_p": 0.9}, absent: []string{"reasoning_effort"}},
		{name: "unset", absent: []string{"thinking", "temperature", "top_p", "reasoning_effort"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				server, body := newWireServer(t, stream)
				client := openai.New("k", append([]openai.Option{openai.WithBaseURL(server.URL)}, tc.opts...)...)
				req := contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "hi"}},
					Thinking: tc.thinking, Effort: tc.effort}
				if tc.sampling {
					req.Temperature, req.TopP = &temperature, &topP
				}
				if stream {
					runStream(t, client, req)
				} else {
					runChat(t, client, req)
				}
				wire := wireBody(t, body())
				for field, want := range tc.want {
					got := wire[field]
					if field == "thinking" {
						got, _ = thinkingType(t, body())
					}
					if got != want {
						t.Fatalf("stream=%v %s = %#v, want %#v; body=%v", stream, field, got, want, wire)
					}
				}
				for _, field := range tc.absent {
					if _, present := wire[field]; present {
						t.Fatalf("stream=%v %s sent although unset: %v", stream, field, wire)
					}
				}
			}
		})
	}
}

// A thinking mode the adapter cannot send as asked is refused before the
// request leaves the client.
func TestRequestThinkingIsRefusedRatherThanUndone(t *testing.T) {
	server, body := newWireServer(t, false)
	for _, tc := range []struct {
		name string
		opts []openai.Option
		req  contract.ChatRequest
		want string
	}{
		{name: "unknown mode", req: contract.ChatRequest{Thinking: "auto"}, want: "unknown thinking mode"},
		{name: "enabled on a tool request of a client that disables it with tools",
			opts: []openai.Option{openai.WithThinkingControl("enabled", true)},
			req:  chatReq(true, 0.5), want: "disables thinking with tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := openai.New("k", append([]openai.Option{openai.WithBaseURL(server.URL)}, tc.opts...)...)
			tc.req.Model, tc.req.Messages = "m", []contract.Message{{Role: "user", Content: "hi"}}
			if tc.req.Thinking == "" {
				tc.req.Thinking = contract.ThinkingEnabled
			}
			_, chatErr := client.Chat(context.Background(), tc.req)
			_, streamErr := client.Stream(context.Background(), tc.req)
			for _, err := range []error{chatErr, streamErr} {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
			}
			if sent := body(); len(sent) != 0 {
				t.Fatalf("refused request reached the server: %s", sent)
			}
		})
	}
}

func TestProtocolObservationReportsTopP(t *testing.T) {
	server, _ := newWireServer(t, false)
	topP := 0.5
	observed := make(chan contract.ProtocolObservation, 1)
	ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observed <- value })
	client := openai.New("k", openai.WithBaseURL(server.URL))
	if _, err := client.Chat(ctx, contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "hi"}},
		TopP: &topP, Thinking: contract.ThinkingDisabled}); err != nil {
		t.Fatal(err)
	}
	options := (<-observed).RequestOptions
	if options == nil || !options.TopP || options.Thinking != "disabled" {
		t.Fatalf("request options = %+v", options)
	}
}
