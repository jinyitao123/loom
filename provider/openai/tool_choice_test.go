package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

func toolChoiceOnWire(t *testing.T, body []byte) (json.RawMessage, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}
	value, ok := fields["tool_choice"]
	return value, ok
}

func TestToolChoiceWireMapping(t *testing.T) {
	for _, test := range []struct {
		name   string
		choice *contract.ToolChoice
		want   string
	}{
		{name: "auto", choice: &contract.ToolChoice{Mode: contract.ToolChoiceAuto}, want: `"auto"`},
		{name: "none", choice: &contract.ToolChoice{Mode: contract.ToolChoiceNone}, want: `"none"`},
		{name: "required", choice: &contract.ToolChoice{Mode: contract.ToolChoiceRequired}, want: `"required"`},
		{name: "named", choice: &contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: "lookup"}, want: `{"type":"function","function":{"name":"lookup"}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(test.name, func(t *testing.T) {
				server, capture := newWireServer(t, stream)
				client := openai.New("test-key", openai.WithBaseURL(server.URL))
				request := chatReq(true, 0.7)
				request.ToolChoice = test.choice
				if stream {
					runStream(t, client, request)
				} else {
					runChat(t, client, request)
				}
				value, ok := toolChoiceOnWire(t, capture())
				if !ok || string(value) != test.want {
					t.Fatalf("tool_choice = %s (present %v), want %s", value, ok, test.want)
				}
			})
		}
	}
}

func TestToolChoiceNilLeavesKeyAbsent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		server, capture := newWireServer(t, stream)
		client := openai.New("test-key", openai.WithBaseURL(server.URL), openai.WithToolChoiceModes(contract.ToolChoiceAuto))
		if stream {
			runStream(t, client, chatReq(true, 0.7))
		} else {
			runChat(t, client, chatReq(true, 0.7))
		}
		if value, ok := toolChoiceOnWire(t, capture()); ok {
			t.Fatalf("nil tool choice reached the wire: %s", value)
		}
	}
}

func TestToolChoiceRejectedBeforeSend(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	for _, test := range []struct {
		name    string
		tools   bool
		choice  contract.ToolChoice
		modes   []contract.ToolChoiceMode
		wantErr error
	}{
		{name: "no offered tools", choice: contract.ToolChoice{Mode: contract.ToolChoiceRequired}, wantErr: openai.ErrToolChoiceInvalid},
		{name: "named tool not offered", tools: true, choice: contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: "other"}, wantErr: openai.ErrToolChoiceInvalid},
		{name: "named tool without name", tools: true, choice: contract.ToolChoice{Mode: contract.ToolChoiceTool}, wantErr: openai.ErrToolChoiceInvalid},
		{name: "name on generic mode", tools: true, choice: contract.ToolChoice{Mode: contract.ToolChoiceRequired, Name: "lookup"}, wantErr: openai.ErrToolChoiceInvalid},
		{name: "unknown mode", tools: true, choice: contract.ToolChoice{Mode: "any"}, wantErr: openai.ErrToolChoiceInvalid},
		{name: "undeclared mode", tools: true, choice: contract.ToolChoice{Mode: contract.ToolChoiceRequired}, modes: []contract.ToolChoiceMode{contract.ToolChoiceAuto, contract.ToolChoiceNone}, wantErr: openai.ErrToolChoiceUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := []openai.Option{openai.WithBaseURL(server.URL)}
			if test.modes != nil {
				options = append(options, openai.WithToolChoiceModes(test.modes...))
			}
			client := openai.New("test-key", options...)
			request := chatReq(test.tools, 0.7)
			choice := test.choice
			request.ToolChoice = &choice
			if _, err := client.Chat(context.Background(), request); !errors.Is(err, test.wantErr) {
				t.Fatalf("Chat error = %v, want %v", err, test.wantErr)
			}
			if stream, err := client.Stream(context.Background(), request); stream != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("Stream = %v, %v; want %v", stream, err, test.wantErr)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid tool choice reached the provider %d times", hits.Load())
	}
}

func TestProtocolObserverRecordsWireOptions(t *testing.T) {
	server, capture := newWireServer(t, false)
	observations := make(chan contract.ProtocolObservation, 1)
	ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observations <- value })
	client := openai.New("test-key", openai.WithBaseURL(server.URL), openai.WithThinkingControl("enabled", true), openai.WithJSONObjectMode())
	request := chatReq(true, 0.7)
	schema := json.RawMessage(`{"type":"object"}`)
	request.Schema = &schema
	request.Messages[0].Content = "answer in JSON"
	request.Effort = contract.EffortHigh
	request.ToolChoice = &contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: "lookup"}
	if _, err := client.Chat(ctx, request); err != nil {
		t.Fatal(err)
	}
	observation := <-observations
	want := contract.ProtocolRequestOptions{Thinking: "disabled", ReasoningEffort: "none", ResponseFormat: "json_object", ToolChoice: "tool"}
	if observation.RequestOptions == nil || *observation.RequestOptions != want {
		t.Fatalf("request options = %#v, want %#v", observation.RequestOptions, want)
	}
	if body := capture(); len(body) == 0 {
		t.Fatal("request was not sent")
	}
}

func TestProtocolObserverCountsTextCallCarriers(t *testing.T) {
	marker := "｜DSML｜"
	split := len("<") + 4 // cut the marker across two deltas
	text := "<" + marker + " calls>"
	wire := protocolFrame(map[string]any{"reasoning_content": "thinking"}, nil) +
		protocolFrame(map[string]any{"content": "done.\n\n" + text[:split]}, nil) +
		protocolFrame(map[string]any{"content": text[split:] + "<" + marker + " invoke>"}, "stop") +
		"data: [DONE]\n\n"
	chunks, observation := observedStream(t, wire, openai.ProtocolObserverOptions{})
	content := ""
	for _, chunk := range chunks {
		content += chunk.Content
	}
	if observation.Content == nil {
		t.Fatal("content observation missing")
	}
	got := *observation.Content
	want := contract.ProtocolContentObservation{ContentBytes: len(content), TextToolProtocolMarkers: 2, ReasoningFrames: 1, ReasoningBytes: len("thinking")}
	if got != want {
		t.Fatalf("content observation = %#v, want %#v", got, want)
	}
	if observation.EmittedCalls != 0 || observation.AssembledCalls != 0 {
		t.Fatalf("text markup became calls: %#v", observation)
	}
}
