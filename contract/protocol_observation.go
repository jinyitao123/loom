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
}

// ToolArgumentObservation describes assembled bytes at a wire index (or array
// position for non-streaming responses). SHA256 is over those bytes, including
// incomplete argument fragments on an abnormal stream termination.
type ToolArgumentObservation struct {
	Index  int    `json:"index"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}
