package main

import (
	"github.com/jinyitao123/loom/contract" // LLM / StreamChunk 等契约类型
	"github.com/jinyitao123/loom/stdlib"   // NewStreamingLLM：流式装饰器的唯一实现
)

// newStreamingLLM wraps a contract.LLM so that Chat() drives the provider's
// token Stream and emits each text delta on the sink as it arrives — turning
// loom's blocking tool-loop into a streaming one without touching the engine.
// The streaming semantics (stream errors, truncated streams, finish reasons,
// the blocking-Chat fallback) live in stdlib.NewStreamingLLM; this file only
// adapts the stdout-json sink to contract.StreamSink.
// 中文补充：流式语义（错误块、截断、结束原因、阻塞回退）统一由
// stdlib.NewStreamingLLM 实现；这里只把 stdout-json 事件输出端适配为
// contract.StreamSink，文本增量写成 text 事件，终止块不单独输出事件。
func newStreamingLLM(inner contract.LLM, out sink) contract.LLM {
	return stdlib.NewStreamingLLM(inner, textSink{out: out})
}

// textSink forwards text deltas to the event sink; Done chunks and custom
// events carry nothing the stdout-json protocol emits here.
type textSink struct{ out sink }

func (t textSink) SendChunk(chunk contract.StreamChunk) error {
	if chunk.Content == "" {
		return nil
	}
	return t.out.Text(chunk.Content)
}

func (textSink) SendEvent(string, any) error { return nil }
func (textSink) Close() error                { return nil }
