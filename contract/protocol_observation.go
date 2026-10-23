package contract

// ProtocolObservation is a bounded provider-boundary value, separate from an
// LLM response. Absence means not collected. Counts in an incomplete observation
// cannot prove that no tool call occurred. It contains no messages, raw frames,
// headers, tool names, call IDs, argument text or reasoning content.
type ProtocolObservation struct {
	Version            int                       `json:"version"`
	Protocol           string                    `json:"protocol"`
	Complete           bool                      `json:"complete"`
	End                string                    `json:"end"`
	RequestSent        bool                      `json:"request_sent"`
	RequestToolCount   int                       `json:"request_tool_count"`
	RequestToolsSHA256 string                    `json:"request_tools_sha256,omitempty"`
	DataFrames         int                       `json:"data_frames"`
	ParseFailures      int                       `json:"parse_failures"`
	ToolDeltas         int                       `json:"tool_deltas"`
	ToolIndexes        []int                     `json:"tool_indexes"`
	AssembledCalls     int                       `json:"assembled_calls"`
	EmittedCalls       int                       `json:"emitted_calls"`
	FinishReason       string                    `json:"finish_reason"`
	DoneSeen           bool                      `json:"done_seen"`
	Arguments          []ToolArgumentObservation `json:"arguments"`
	Truncated          bool                      `json:"truncated"`
	// RequestOptions are the option values actually written to the wire. Nil
	// means the request was rejected before its wire form was built.
	RequestOptions *ProtocolRequestOptions `json:"request_options,omitempty"`
	// Content counts response text carriers. Nil means not collected; zero
	// counts in a present value mean the carrier was observed and empty.
	Content *ProtocolContentObservation `json:"content,omitempty"`
	// RequestBytes is the size of the request body this attempt built.
	RequestBytes int `json:"request_bytes,omitempty"`
	// HTTPStatus is the provider's response status, 200 included; zero means
	// no response arrived.
	HTTPStatus int `json:"http_status,omitempty"`
	// ProviderRequestID is the provider's identifier for this request, taken
	// only from an allowlisted response header (x-request-id, request-id) and
	// only when short and plain; empty means none was observed.
	ProviderRequestID string `json:"provider_request_id,omitempty"`
}

// ProtocolRequestOptions records enum values only. Empty means the option was
// omitted from the wire request; "other" means an unrecognized value was sent.
type ProtocolRequestOptions struct {
	Thinking        string `json:"thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ResponseFormat  string `json:"response_format,omitempty"`
	ToolChoice      string `json:"tool_choice,omitempty"`
	// OutputLimit names the field that carried the output limit
	// (max_tokens or max_completion_tokens); empty when none was sent.
	OutputLimit string `json:"output_limit,omitempty"`
	// Temperature reports that a sampling temperature was sent; its value is
	// not recorded.
	Temperature bool `json:"temperature,omitempty"`
	// Model is the model name written to the wire; "other" when it is not a
	// short plain name.
	Model string `json:"model,omitempty"`
	// Stream reports a streamed request.
	Stream bool `json:"stream,omitempty"`
	// OutputLimitValue is the limit sent in OutputLimit's field.
	OutputLimitValue int `json:"output_limit_value,omitempty"`
}

// ProtocolContentObservation counts bytes and markers without retaining text.
// TextToolProtocolMarkers counts provider tool-call markup found in ordinary
// content; such text is not an executable call. Reasoning is counted, never kept.
type ProtocolContentObservation struct {
	ContentBytes            int `json:"content_bytes"`
	TextToolProtocolMarkers int `json:"text_tool_protocol_markers"`
	ReasoningFrames         int `json:"reasoning_frames"`
	ReasoningBytes          int `json:"reasoning_bytes"`
}

// ToolArgumentObservation describes assembled bytes at a wire index (or array
// position for non-streaming responses). SHA256 is over those bytes, including
// incomplete argument fragments on an abnormal stream termination.
type ToolArgumentObservation struct {
	Index  int    `json:"index"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}
