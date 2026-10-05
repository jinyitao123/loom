package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptrace"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/jinyitao123/loom/contract"
)

// ProtocolObserverOptions bounds metadata, never the model request or response.
// Non-positive limits use defaults; larger limits are clamped to hard ceilings.
type ProtocolObserverOptions struct {
	MaxDataFrames  int
	MaxToolDeltas  int
	MaxToolIndexes int
}

type protocolObserverKey struct{}
type protocolObserver struct {
	options ProtocolObserverOptions
	observe func(contract.ProtocolObservation)
}

// WithProtocolObserver opts this context into one callback per Client.Chat or
// Client.Stream invocation, including failed attempts. Streaming callbacks run
// before the returned channel closes. A nil callback disables collection.
// Observers must return promptly and synchronize shared state across concurrent
// calls. A callback panic is isolated from model execution. No request/response
// or journal JSON fields are added; hosts own activation, identity and storage.
func WithProtocolObserver(ctx context.Context, options ProtocolObserverOptions, observe func(contract.ProtocolObservation)) context.Context {
	if observe == nil {
		if _, present := ctx.Value(protocolObserverKey{}).(protocolObserver); present {
			return context.WithValue(ctx, protocolObserverKey{}, protocolObserver{})
		}
		return ctx
	}
	return context.WithValue(ctx, protocolObserverKey{}, protocolObserver{options, observe})
}

type protocolCapture struct {
	observer protocolObserver
	value    contract.ProtocolObservation
	indexes  map[int]bool
	frames   int
	deltas   int
	partial  bool
	sent     atomic.Bool
	once     sync.Once
}

func observationLimit(value, fallback, ceiling int) int {
	if value <= 0 {
		return fallback
	}
	if value > ceiling {
		return ceiling
	}
	return value
}

func newProtocolCapture(ctx context.Context, protocol string) *protocolCapture {
	observer, ok := ctx.Value(protocolObserverKey{}).(protocolObserver)
	if !ok || observer.observe == nil {
		return nil
	}
	observer.options.MaxDataFrames = observationLimit(observer.options.MaxDataFrames, 16384, 65536)
	observer.options.MaxToolDeltas = observationLimit(observer.options.MaxToolDeltas, 4096, 65536)
	observer.options.MaxToolIndexes = observationLimit(observer.options.MaxToolIndexes, 128, 256)
	return &protocolCapture{observer: observer, indexes: map[int]bool{}, value: contract.ProtocolObservation{
		Version: 1, Protocol: protocol, End: "before_send", FinishReason: "unknown",
		ToolIndexes: []int{}, Arguments: []contract.ToolArgumentObservation{},
	}}
}

func (p *protocolCapture) request(tools []oaiTool) {
	if p == nil {
		return
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		p.partial = true
		return
	}
	p.value.RequestToolCount = len(tools)
	p.value.RequestToolsSHA256 = observationSHA256(raw)
}

func (p *protocolCapture) trace(ctx context.Context) context.Context {
	if p == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			p.sent.Store(true)
		}
	}})
}

func (p *protocolCapture) frame() {
	if p == nil {
		return
	}
	p.frames++
	if p.frames > p.observer.options.MaxDataFrames {
		p.value.Truncated = true
		return
	}
	p.value.DataFrames++
}

func (p *protocolCapture) parseFailure() {
	if p != nil {
		p.partial = true
		if p.frames <= p.observer.options.MaxDataFrames {
			p.value.ParseFailures++
		}
	}
}

// Successful JSON decoding alone does not prove that a frame used the stream
// shape this adapter understands. Unknown shapes must not look like zero calls.
func (p *protocolCapture) streamShape(raw []byte, chunk oaiStreamChunk) {
	if p == nil || p.frames > p.observer.options.MaxDataFrames {
		return
	}
	var frame struct {
		Choices []struct {
			Delta json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &frame) != nil || len(frame.Choices) > 1 || len(frame.Choices) == 0 && chunk.Usage == nil {
		p.parseFailure()
		return
	}
	if len(frame.Choices) == 1 {
		var delta map[string]json.RawMessage
		if json.Unmarshal(frame.Choices[0].Delta, &delta) != nil || delta == nil {
			p.parseFailure()
			return
		}
		if unsupportedMessageFields(delta) {
			p.parseFailure()
		}
	}
}

func (p *protocolCapture) chatShape(raw []byte) {
	if p == nil {
		return
	}
	var response struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Choices) != 1 || response.Choices[0].Message == nil || unsupportedMessageFields(response.Choices[0].Message) {
		p.parseFailure()
	}
}

// Only tool_calls is an executable call carrier. Unknown non-null fields could
// contain ignored calls; recognized non-call fields are never copied out.
func unsupportedMessageFields(fields map[string]json.RawMessage) bool {
	for key, value := range fields {
		switch key {
		case "content", "tool_calls", "role", "reasoning_content", "refusal":
			continue
		default:
			if string(value) != "null" {
				return true
			}
		}
	}
	return false
}

func (p *protocolCapture) delta(index int) {
	if p == nil {
		return
	}
	p.deltas++
	if p.frames > p.observer.options.MaxDataFrames || p.deltas > p.observer.options.MaxToolDeltas {
		p.value.Truncated = true
		return
	}
	p.value.ToolDeltas++
	if p.indexes[index] {
		return
	}
	if len(p.indexes) == p.observer.options.MaxToolIndexes {
		p.value.Truncated = true
		return
	}
	p.indexes[index] = true
	if index < 0 {
		p.partial = true
	}
}

func (p *protocolCapture) calls(calls map[int]*contract.ToolCall) {
	if p == nil {
		return
	}
	p.value.AssembledCalls = len(calls)
	indexes := sortedToolIndexes(calls)
	for position, index := range indexes {
		if position != index || calls[index].ID == "" || calls[index].Name == "" {
			p.partial = true
		}
	}
	for index := range p.indexes {
		p.value.ToolIndexes = append(p.value.ToolIndexes, index)
	}
	sort.Ints(p.value.ToolIndexes)
	for _, index := range p.value.ToolIndexes {
		if call := calls[index]; call != nil {
			p.value.Arguments = append(p.value.Arguments, contract.ToolArgumentObservation{
				Index: index, Bytes: len(call.Args), SHA256: observationSHA256([]byte(call.Args)),
			})
		}
	}
}

func (p *protocolCapture) end(end, finishReason string, doneSeen bool, emitted int) {
	if p == nil {
		return
	}
	p.value.End, p.value.DoneSeen, p.value.EmittedCalls = end, doneSeen, emitted
	switch finishReason {
	case "stop", "tool_calls", "length", "content_filter", "function_call":
		p.value.FinishReason = finishReason
	default:
		p.value.FinishReason = "unknown"
	}
	p.value.Complete = (end == "response" || end == "done" && doneSeen) &&
		!p.partial && !p.value.Truncated && p.value.FinishReason != "unknown"
}

func (p *protocolCapture) emit() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.value.RequestSent = p.sent.Load()
		if (p.value.FinishReason == "tool_calls" || p.value.FinishReason == "function_call") && p.value.AssembledCalls == 0 {
			p.partial = true
		}
		p.value.Complete = p.value.Complete && p.value.RequestSent && !p.partial && !p.value.Truncated
		defer func() { _ = recover() }()
		p.observer.observe(p.value)
	})
}

func protocolTransportEnd(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancel"
	}
	return "transport_error"
}

func observationSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func sortedToolIndexes(calls map[int]*contract.ToolCall) []int {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}
