package stdlib

import (
	"context"
	"strings"
)

// BareToolProtocolCompletionPolicyID identifies the recognition policy used by
// RejectBareToolProtocolCompletion. Hosts composing verifiers must also include
// the identity of their other acceptance rules in CompletionVerifierID.
const BareToolProtocolCompletionPolicyID = "loom.reject-bare-tool-protocol.v2"

// BareToolProtocolRejectionReason is the CompletionDecision.Reason used when
// RejectBareToolProtocolCompletion rejects a bare text tool-call block. It does
// not change which candidates are accepted, so the policy ID above is unchanged.
const BareToolProtocolRejectionReason = "bare_tool_protocol"

// RejectBareToolProtocolCompletion wraps an optional completion verifier with
// an opt-in check for a bare, balanced DSML calls/invoke block ending the final
// text. The block must begin a separate paragraph, with or without a preamble. Such
// text is not a dispatched tool call or a tool result. Rejection uses the normal
// ToolLoop completion feedback and round budget; this verifier never executes,
// removes, or repairs the text itself.
//
// Prose mentioning tags, quotations and code fences containing protocol examples
// are accepted by this check. A prose introduction does not exempt a trailing
// bare block: hosts requesting such examples should disable this policy or ask
// for quoted/fenced examples. It does not infer the author's intent, establish task quality or
// require tools to have been called. A nil next accepts other text unchanged.
func RejectBareToolProtocolCompletion(next CompletionVerifier) CompletionVerifier {
	return CompletionVerifierFunc(func(ctx context.Context, candidate CompletionCandidate) (CompletionDecision, error) {
		if isBareToolProtocol(candidate.Content) {
			return CompletionDecision{Feedback: "The proposed final response ends with a bare tool-call protocol block in text. It did not execute a tool and is not a tool result. If a tool is needed, use the provided structured tool-call interface and wait for its actual result. Otherwise provide the requested answer using the available evidence, without a bare protocol block. If explaining protocol syntax is the requested task, quote or fence the example.", Reason: BareToolProtocolRejectionReason}, nil
		}
		if next != nil {
			return next.VerifyCompletion(ctx, candidate)
		}
		return CompletionDecision{Accepted: true}, nil
	})
}

// Match balanced trailing blocks only, not mentions or quoted Markdown examples.
// Both spellings have distinct literal delimiters; arbitrary XML-like text is
// outside this narrow policy. The body is deliberately not decoded into calls.
func isBareToolProtocol(content string) bool {
	content = strings.TrimRight(content, " \t\r\n")
	for _, marker := range []string{"｜DSML｜", "｜｜DSML｜｜"} {
		if !strings.HasSuffix(content, "</"+marker+" calls>") && !strings.HasSuffix(content, "</"+marker+" invoke>") {
			continue
		}
		for offset := 0; offset < len(content); {
			found := strings.Index(content[offset:], "<"+marker+" ")
			if found < 0 {
				break
			}
			start := offset + found
			if independentProtocolParagraph(content, start) && balancedProtocolBlock(content[start:], marker) {
				return true
			}
			offset = start + 1
		}
	}
	return false
}

func independentProtocolParagraph(content string, start int) bool {
	lineStart := strings.LastIndex(content[:start], "\n") + 1
	indent := content[lineStart:start]
	// Four spaces or a tab introduce an indented Markdown code block. Quote
	// prefixes and inline examples also fail this plain paragraph boundary.
	if len(indent) > 3 || strings.Trim(indent, " ") != "" {
		return false
	}
	if lineStart > 0 {
		previous := content[:lineStart-1]
		previous = previous[strings.LastIndex(previous, "\n")+1:]
		if strings.TrimSpace(previous) != "" {
			return false
		}
	}
	return !openMarkdownFence(content[:lineStart])
}

func openMarkdownFence(prefix string) bool {
	var fence byte
	var width int
	for _, line := range strings.Split(prefix, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
			continue
		}
		n := 1
		for n < len(trimmed) && trimmed[n] == trimmed[0] {
			n++
		}
		if n < 3 {
			continue
		}
		if fence == 0 {
			fence, width = trimmed[0], n
		} else if trimmed[0] == fence && n >= width && strings.TrimSpace(trimmed[n:]) == "" {
			fence, width = 0, 0
		}
	}
	return fence != 0
}

func balancedProtocolBlock(block, marker string) bool {
	var stack []string
	for offset := 0; offset < len(block); {
		opening := strings.Index(block[offset:], "<"+marker+" ")
		closing := strings.Index(block[offset:], "</"+marker+" ")
		isClose := closing >= 0 && (opening < 0 || closing < opening)
		found := opening
		prefix := "<" + marker + " "
		if isClose {
			found, prefix = closing, "</"+marker+" "
		}
		if found < 0 || (len(stack) == 0 && offset+found != 0) {
			return false
		}
		start := offset + found + len(prefix)
		end := strings.IndexByte(block[start:], '>')
		if end < 0 {
			return false
		}
		end += start
		fields := strings.Fields(block[start:end])
		if len(fields) == 0 || (fields[0] != "calls" && fields[0] != "invoke" && fields[0] != "parameter") {
			return false
		}
		name := fields[0]
		if isClose {
			if len(fields) != 1 || len(stack) == 0 || stack[len(stack)-1] != name {
				return false
			}
			stack = stack[:len(stack)-1]
		} else {
			if len(stack) == 0 && name != "calls" && name != "invoke" {
				return false
			}
			stack = append(stack, name)
		}
		offset = end + 1
		if len(stack) == 0 {
			return strings.TrimSpace(block[offset:]) == ""
		}
	}
	return false
}
