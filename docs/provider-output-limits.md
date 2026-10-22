# Chat Completions output limits

`contract.ChatRequest.MaxTokens` is the per-request output limit. The OpenAI
adapter sends it as `max_tokens` by default. A host whose endpoint requires
`max_completion_tokens` constructs its client with `openai.WithMaxCompletionTokens()`.
The choice is explicit; model names do not select a dialect. Chat and Stream
send exactly one of these fields. A zero limit omits both fields.

The opt-in changes only request serialization. It does not raise a token budget,
select a reasoning level, or retry a failed request with different parameters.
The protocol observer reports the actual field name in `request_options.output_limit`
and whether a temperature was sent in `request_options.temperature`; it retains
neither the sampling value nor prompts, credentials, or hidden reasoning.

Purity proof: this changes a Layer-1 additive observation value and the provider
leaf adapter. It adds no business vocabulary and leaves default requests intact.
Steps still return deltas, the engine still owns merging, enforcement remains
opt-in, and deterministic single-hop routing is unchanged.
