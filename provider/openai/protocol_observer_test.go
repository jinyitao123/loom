package openai_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

func observedStream(t *testing.T, wire string, options openai.ProtocolObserverOptions) ([]contract.StreamChunk, contract.ProtocolObservation) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(server.Close)
	observations := make(chan contract.ProtocolObservation, 2)
	ctx := openai.WithProtocolObserver(context.Background(), options, func(value contract.ProtocolObservation) { observations <- value })
	client := openai.New("probe-test-key", openai.WithBaseURL(server.URL))
	stream, err := client.Stream(ctx, chatReq(true, 0.7))
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectStream(t, stream)
	select {
	case observation := <-observations:
		select {
		case <-observations:
			t.Fatal("more than one observation for one provider attempt")
		default:
		}
		return chunks, observation
	default:
		t.Fatal("observation was not emitted before stream close")
		return nil, contract.ProtocolObservation{}
	}
}

func protocolFrame(delta any, reason any) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": reason}}})
	return "data: " + string(raw) + "\n\n"
}

func protocolCall(index int, id, name, args string) map[string]any {
	return map[string]any{"index": index, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func protocolDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestProtocolObserverStreamBoundaries(t *testing.T) {
	call := func(index int) string {
		return protocolFrame(map[string]any{"tool_calls": []any{protocolCall(index, "call", "lookup", `{"q":1}`)}}, "tool_calls")
	}
	for _, test := range []struct {
		name, wire, end                              string
		complete                                     bool
		frames, failures, deltas, assembled, emitted int
		indexes                                      []int
		streamError                                  bool
	}{
		{name: "zero calls", wire: protocolFrame(map[string]any{"content": "ok"}, "stop") + "data: [DONE]\n\n", end: "done", complete: true, frames: 1, indexes: []int{}},
		{name: "one call", wire: call(0) + "data: [DONE]\n\n", end: "done", complete: true, frames: 1, deltas: 1, assembled: 1, emitted: 1, indexes: []int{0}},
		{name: "fragmented arguments", wire: protocolFrame(map[string]any{"tool_calls": []any{protocolCall(0, "call", "lookup", `{"q":`)}}, nil) + protocolFrame(map[string]any{"tool_calls": []any{protocolCall(0, "", "", `1}`)}}, "tool_calls") + "data: [DONE]\n\n", end: "done", complete: true, frames: 2, deltas: 2, assembled: 1, emitted: 1, indexes: []int{0}},
		{name: "multiple calls", wire: protocolFrame(map[string]any{"tool_calls": []any{protocolCall(1, "second", "lookup", `{"q":2}`), protocolCall(0, "first", "lookup", `{"q":1}`)}}, "tool_calls") + "data: [DONE]\n\n", end: "done", complete: true, frames: 1, deltas: 2, assembled: 2, emitted: 2, indexes: []int{0, 1}},
		{name: "sparse index preserved", wire: call(5) + "data: [DONE]\n\n", end: "done", frames: 1, deltas: 1, assembled: 1, emitted: 1, indexes: []int{5}},
		{name: "negative index incomplete", wire: call(-1) + "data: [DONE]\n\n", end: "done", frames: 1, deltas: 1, assembled: 1, emitted: 1, indexes: []int{-1}},
		{name: "call finish without calls", wire: protocolFrame(map[string]any{"content": "ok"}, "tool_calls") + "data: [DONE]\n\n", end: "done", frames: 1, indexes: []int{}},
		{name: "malformed frame", wire: "data: {malformed\n\n" + protocolFrame(map[string]any{"content": "ok"}, "stop") + "data: [DONE]\n\n", end: "done", frames: 2, failures: 1, indexes: []int{}},
		{name: "unsupported stream shape", wire: protocolFrame(map[string]any{"content": "ok"}, "stop") + "data: {\"choices\":[{\"message\":{\"tool_calls\":[]}}]}\n\ndata: [DONE]\n\n", end: "done", frames: 2, failures: 1, indexes: []int{}},
		{name: "legacy function call unsupported", wire: protocolFrame(map[string]any{"content": "ok", "function_call": map[string]any{"name": "lookup", "arguments": `{}`}}, "stop") + "data: [DONE]\n\n", end: "done", frames: 1, failures: 1, indexes: []int{}},
		{name: "future call carrier unsupported", wire: protocolFrame(map[string]any{"content": "ok", "future_call": map[string]any{"name": "lookup", "arguments": `{}`}}, "stop") + "data: [DONE]\n\n", end: "done", frames: 1, failures: 1, indexes: []int{}},
		{name: "null legacy call", wire: protocolFrame(map[string]any{"content": "ok", "function_call": nil}, "stop") + "data: [DONE]\n\n", end: "done", complete: true, frames: 1, indexes: []int{}},
		{name: "known non-call fields", wire: protocolFrame(map[string]any{"content": "ok", "role": "assistant", "reasoning_content": "private reasoning", "refusal": nil}, "stop") + "data: [DONE]\n\n", end: "done", complete: true, frames: 1, indexes: []int{}},
		{name: "EOF without done", wire: call(0), end: "eof", frames: 1, deltas: 1, assembled: 1, indexes: []int{0}, streamError: true},
		{name: "unknown reason", wire: protocolFrame(map[string]any{"content": "ok"}, "private-finish-value") + "data: [DONE]\n\n", end: "done", frames: 1, indexes: []int{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunks, observation := observedStream(t, test.wire, openai.ProtocolObserverOptions{})
			emitted := 0
			for _, chunk := range chunks {
				emitted += len(chunk.ToolCalls)
			}
			if observation.Version != 1 || observation.Protocol != "openai_sse" || !observation.RequestSent || observation.RequestToolCount != 1 || len(observation.RequestToolsSHA256) != 64 ||
				observation.Complete != test.complete || observation.End != test.end || observation.DataFrames != test.frames || observation.ParseFailures != test.failures || observation.ToolDeltas != test.deltas ||
				observation.AssembledCalls != test.assembled || observation.EmittedCalls != test.emitted || emitted != observation.EmittedCalls || !reflect.DeepEqual(observation.ToolIndexes, test.indexes) || observation.DoneSeen != (test.end == "done") {
				t.Fatalf("observation = %+v, delivered calls = %d", observation, emitted)
			}
			if (firstErr(t, chunks) != nil) != test.streamError {
				t.Fatalf("unexpected stream error: %v", firstErr(t, chunks))
			}
			if test.assembled == 1 && (len(observation.Arguments) != 1 || observation.Arguments[0].Bytes != 7 || observation.Arguments[0].SHA256 != protocolDigest(`{"q":1}`)) {
				t.Fatalf("argument metadata = %+v", observation.Arguments)
			}
		})
	}
}

func TestProtocolObserverMetadataLimits(t *testing.T) {
	wire := protocolFrame(map[string]any{"tool_calls": []any{protocolCall(0, "first", "lookup", `{}`), protocolCall(1, "second", "lookup", `{}`)}}, "tool_calls") + "data: [DONE]\n\n"
	for _, options := range []openai.ProtocolObserverOptions{{MaxToolIndexes: 1}, {MaxToolDeltas: 1}, {MaxDataFrames: 1}} {
		actualWire := wire
		if options.MaxDataFrames == 1 {
			actualWire = protocolFrame(map[string]any{"content": "preface"}, nil) + wire
		}
		chunks, observation := observedStream(t, actualWire, options)
		if observation.Complete || !observation.Truncated || observation.AssembledCalls != 2 || observation.EmittedCalls != 2 || len(observation.ToolIndexes) > 1 || len(observation.Arguments) > 1 || options.MaxToolDeltas == 1 && observation.ToolDeltas > 1 || options.MaxDataFrames == 1 && observation.DataFrames > 1 {
			t.Fatalf("bounded observation = %+v", observation)
		}
		if firstErr(t, chunks) != nil {
			t.Fatal("metadata limits changed model execution")
		}
	}
}

func TestProtocolObserverChatPrivacyAndWireIdentity(t *testing.T) {
	const secret = "probe-header-secret"
	const private = "private-message-and-arguments"
	var bodies [][]byte
	var bodiesMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, body)
		bodiesMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"private-response","reasoning_content":"private-reasoning","tool_calls":[{"id":"private-call-id","type":"function","function":{"name":"private-tool-name","arguments":"\"private-message-and-arguments\""}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer server.Close()
	client := openai.New(secret, openai.WithBaseURL(server.URL))
	request := contract.ChatRequest{Model: "test", Messages: []contract.Message{{Role: "user", Content: private}}, Tools: []contract.ToolDef{{Name: "private-tool-name", Description: "private-description", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	plain, err := client.Chat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var observations []contract.ProtocolObservation
	ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observations = append(observations, value) })
	probed, err := client.Chat(ctx, request)
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if err != nil || !reflect.DeepEqual(plain, probed) || len(observations) != 1 || len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("probe changed request/response: err=%v observations=%d", err, len(observations))
	}
	observation := observations[0]
	if !observation.Complete || observation.Protocol != "openai_chat" || observation.End != "response" || observation.DoneSeen || observation.AssembledCalls != 1 || observation.EmittedCalls != 1 || len(observation.Arguments) != 1 || observation.Arguments[0].SHA256 != protocolDigest(`"`+private+`"`) {
		t.Fatalf("chat observation = %+v", observation)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(bodies[1], &sent); err != nil || observation.RequestToolsSHA256 != protocolDigest(string(sent["tools"])) {
		t.Fatal("request tool digest was not computed at the wire boundary")
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, private, "private-call-id", "private-tool-name", "private-description", "private-response", "private-reasoning", "Authorization", "reasoning_content"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("observation leaked %q", forbidden)
		}
	}
}

func TestProtocolObserverFailureAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, body, end string
		status          int
	}{
		{"http failure", "private-server-error", "http_error", http.StatusUnauthorized},
		{"malformed response", "{malformed-private-body", "decode_error", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			var values []contract.ProtocolObservation
			ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { values = append(values, value) })
			_, err := openai.New("test-key", openai.WithBaseURL(server.URL)).Chat(ctx, chatReq(true, 0.7))
			if err == nil || len(values) != 1 || values[0].Complete || !values[0].RequestSent || values[0].End != test.end {
				t.Fatalf("failure observation = %+v, err=%v", values, err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, protocolFrame(map[string]any{"content": "partial", "tool_calls": []any{protocolCall(0, "call", "lookup", `{"q":`)}}, nil))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	values := make(chan contract.ProtocolObservation, 2)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := openai.WithProtocolObserver(base, openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { values <- value })
	stream, err := openai.New("test-key", openai.WithBaseURL(server.URL)).Stream(ctx, chatReq(true, 0.7))
	if err != nil {
		t.Fatal(err)
	}
	if chunk := <-stream; chunk.Content != "partial" {
		t.Fatal("expected first content chunk")
	}
	cancel()
	chunks := collectStream(t, stream)
	observation := <-values
	if !errors.Is(firstErr(t, chunks), openai.ErrStreamCancelled) || observation.Complete || observation.End != "cancel" || observation.DoneSeen || observation.EmittedCalls != 0 || observation.AssembledCalls != 1 || len(observation.Arguments) != 1 || observation.Arguments[0].SHA256 != protocolDigest(`{"q":`) {
		t.Fatalf("cancel observation = %+v", observation)
	}
}

func TestProtocolObserverDisabledAndPanicIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client := openai.New("test-key", openai.WithBaseURL(server.URL))
	base := context.Background()
	if ctx := openai.WithProtocolObserver(base, openai.ProtocolObserverOptions{}, nil); ctx != base {
		t.Fatal("nil observer changed context")
	}
	observed := false
	parent := openai.WithProtocolObserver(base, openai.ProtocolObserverOptions{}, func(contract.ProtocolObservation) { observed = true })
	if _, err := client.Chat(openai.WithProtocolObserver(parent, openai.ProtocolObserverOptions{}, nil), chatReq(false, 0.7)); err != nil || observed {
		t.Fatal("explicit disabling inherited an outer observer")
	}
	ctx := openai.WithProtocolObserver(base, openai.ProtocolObserverOptions{}, func(contract.ProtocolObservation) { panic("observer failed") })
	response, err := client.Chat(ctx, chatReq(false, 0.7))
	if err != nil || response.Content != "ok" {
		t.Fatalf("observer panic changed execution: %v", err)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "protocol") {
		t.Fatal("observer changed existing response JSON")
	}
}

func TestProtocolObserverStreamPreSendAndHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, test := range []struct {
		name   string
		cancel bool
		end    string
	}{
		{name: "http", end: "http_error"},
		{name: "cancel before send", cancel: true, end: "cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			var observations []contract.ProtocolObservation
			ctx := openai.WithProtocolObserver(base, openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observations = append(observations, value) })
			stream, err := openai.New("test-key", openai.WithBaseURL(server.URL)).Stream(ctx, chatReq(true, 0.7))
			if err == nil || stream != nil || len(observations) != 1 || observations[0].Complete || observations[0].End != test.end || observations[0].RequestSent == test.cancel {
				t.Fatalf("failed stream observation = %+v, err=%v", observations, err)
			}
		})
	}
}

func TestProtocolObserverChatUnsupportedCallsAreIncomplete(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		complete     bool
	}{
		{name: "legacy call", fields: `,"function_call":{"name":"lookup","arguments":"{}"}`},
		{name: "future carrier", fields: `,"future_call":{"name":"lookup","arguments":"{}"}`},
		{name: "null compatibility", fields: `,"function_call":null`, complete: true},
		{name: "known non-call fields", fields: `,"role":"assistant","reasoning_content":"private","refusal":null`, complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"`+test.fields+`},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			client := openai.New("test-key", openai.WithBaseURL(server.URL))
			plain, err := client.Chat(context.Background(), chatReq(true, 0.7))
			if err != nil {
				t.Fatal(err)
			}
			var observations []contract.ProtocolObservation
			ctx := openai.WithProtocolObserver(context.Background(), openai.ProtocolObserverOptions{}, func(value contract.ProtocolObservation) { observations = append(observations, value) })
			probed, err := client.Chat(ctx, chatReq(true, 0.7))
			if err != nil || !reflect.DeepEqual(plain, probed) || len(observations) != 1 || observations[0].Complete != test.complete || observations[0].AssembledCalls != 0 || observations[0].EmittedCalls != 0 || (observations[0].ParseFailures == 0) != test.complete {
				t.Fatalf("unsupported chat observation = %+v, err=%v", observations, err)
			}
		})
	}
}
