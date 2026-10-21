package interactive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type elicited struct {
	action  string
	content map[string]any
	err     error
}

func (h *harness) elicit(message, schema string) chan elicited {
	out := make(chan elicited, 1)
	go func() {
		action, content, err := h.server.Elicit(context.Background(), "tools", message, json.RawMessage(schema))
		out <- elicited{action, content, err}
	}()
	return out
}

func (h *harness) uiRequest() map[string]any {
	lines := h.until(isType("extension_ui_request"))
	return lines[len(lines)-1]
}

func (h *harness) answer(request map[string]any, reply map[string]any) {
	reply["type"] = "extension_ui_response"
	reply["id"] = request["id"]
	line, _ := json.Marshal(reply)
	if _, err := h.in.Write(append(line, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

func TestElicitSingleChoiceMapsLabelsToValues(t *testing.T) {
	h := newHarness(t, &scriptedLLM{}, false)
	done := h.elicit("Pick a color", `{"type":"object","properties":{"color":{"type":"string","enum":["r","b"],"enumNames":["Red","Blue"]}}}`)
	req := h.uiRequest()
	if req["method"] != "select" || req["title"] != "Pick a color" || len(req["options"].([]any)) != 2 {
		t.Fatalf("request = %v", req)
	}
	h.answer(req, map[string]any{"value": "Blue"})
	got := <-done
	if got.action != "accept" || got.content["color"] != "b" {
		t.Fatalf("result = %+v", got)
	}
}

func TestElicitSeveralChoicesFormAQuestionnaire(t *testing.T) {
	h := newHarness(t, &scriptedLLM{}, false)
	done := h.elicit("Plan the trip", `{"type":"object","properties":{"when":{"type":"string","title":"When","enum":["today","tomorrow"]},"how":{"type":"string","title":"How","oneOf":[{"const":"car","title":"By car"},{"const":"rail","title":"By train"}]}}}`)
	first, second := h.uiRequest(), h.uiRequest()
	for _, req := range []map[string]any{first, second} {
		options := req["options"].([]any)
		if !strings.HasPrefix(options[0].(string), questionnaireMarker) || !strings.HasSuffix(options[0].(string), ":2") {
			t.Fatalf("questionnaire marker missing: %v", options)
		}
	}
	answers := map[string]string{}
	for _, req := range []map[string]any{first, second} {
		if strings.HasSuffix(req["title"].(string), "When") {
			answers["when"] = req["id"].(string)
			h.answer(req, map[string]any{"value": "tomorrow"})
		} else {
			h.answer(req, map[string]any{"value": "By train"})
		}
	}
	got := <-done
	if got.action != "accept" || got.content["when"] != "tomorrow" || got.content["how"] != "rail" {
		t.Fatalf("result = %+v", got)
	}
}

func TestElicitConfirmInputAndCancel(t *testing.T) {
	h := newHarness(t, &scriptedLLM{}, false)

	done := h.elicit("Delete the draft?", `{"type":"object","properties":{}}`)
	req := h.uiRequest()
	if req["method"] != "confirm" {
		t.Fatalf("request = %v", req)
	}
	h.answer(req, map[string]any{"confirmed": false})
	if got := <-done; got.action != "decline" {
		t.Fatalf("declined confirmation = %+v", got)
	}

	done = h.elicit("Details", `{"type":"object","properties":{"count":{"type":"integer","description":"How many"},"urgent":{"type":"boolean"}}}`)
	req = h.uiRequest()
	if req["method"] != "input" || req["placeholder"] != "How many" {
		t.Fatalf("input request = %v", req)
	}
	h.answer(req, map[string]any{"value": " 3 "})
	req = h.uiRequest()
	if req["method"] != "confirm" {
		t.Fatalf("boolean request = %v", req)
	}
	h.answer(req, map[string]any{"confirmed": true})
	if got := <-done; got.action != "accept" || got.content["count"] != int64(3) || got.content["urgent"] != true {
		t.Fatalf("details = %+v", got)
	}

	done = h.elicit("Name?", `{"type":"object","properties":{"name":{"type":"string"}}}`)
	h.answer(h.uiRequest(), map[string]any{"cancelled": true})
	if got := <-done; got.action != "cancel" {
		t.Fatalf("cancelled = %+v", got)
	}
}
