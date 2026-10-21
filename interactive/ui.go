package interactive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// UIAnswer is the host's reply to a dialog.
type UIAnswer struct {
	Value     string
	Confirmed bool
	Cancelled bool
}

// questionnaireMarker prefixes the first option of each select in a group the
// host shows as one questionnaire: "<marker><group>:<index>:<total>".
const questionnaireMarker = "__prime_ask_user__"

// Ask emits an extension_ui_request dialog (select, confirm, input or editor)
// and waits for the matching extension_ui_response.
// Ask 发出 extension_ui_request 对话框并等待对应的 extension_ui_response。
func (s *Server) Ask(ctx context.Context, request map[string]any) (UIAnswer, error) {
	id := newUUID()
	ch := make(chan UIAnswer, 1)
	s.uiMu.Lock()
	if s.ui == nil {
		s.ui = map[string]chan UIAnswer{}
	}
	s.ui[id] = ch
	s.uiMu.Unlock()
	defer func() {
		s.uiMu.Lock()
		delete(s.ui, id)
		s.uiMu.Unlock()
	}()
	event := map[string]any{"type": "extension_ui_request", "id": id}
	for k, v := range request {
		event[k] = v
	}
	s.Emit(event)
	select {
	case answer := <-ch:
		return answer, nil
	case <-ctx.Done():
		return UIAnswer{Cancelled: true}, ctx.Err()
	}
}

func (s *Server) deliverUI(cmd command) {
	s.uiMu.Lock()
	ch := s.ui[cmd.ID]
	s.uiMu.Unlock()
	if ch == nil {
		return
	}
	answer := UIAnswer{Cancelled: cmd.Cancelled}
	if cmd.Value != nil {
		answer.Value = *cmd.Value
	}
	if cmd.Confirmed != nil {
		answer.Confirmed = *cmd.Confirmed
	}
	select {
	case ch <- answer:
	default:
	}
}

type uiState struct {
	uiMu sync.Mutex
	ui   map[string]chan UIAnswer
}

// schemaField is one property of an input request schema.
type schemaField struct {
	name        string
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Enum        []any    `json:"enum"`
	EnumNames   []string `json:"enumNames"`
	OneOf       []struct {
		Const any    `json:"const"`
		Title string `json:"title"`
	} `json:"oneOf"`
}

func (f schemaField) label() string {
	if f.Title != "" {
		return f.Title
	}
	return f.name
}

// choices returns the option labels and values of an enumerated field.
func (f schemaField) choices() ([]string, []any) {
	if len(f.OneOf) > 0 {
		labels := make([]string, len(f.OneOf))
		values := make([]any, len(f.OneOf))
		for i, o := range f.OneOf {
			labels[i], values[i] = o.Title, o.Const
			if labels[i] == "" {
				labels[i] = fmt.Sprint(o.Const)
			}
		}
		return labels, values
	}
	labels := make([]string, len(f.Enum))
	for i, v := range f.Enum {
		labels[i] = fmt.Sprint(v)
		if i < len(f.EnumNames) && f.EnumNames[i] != "" {
			labels[i] = f.EnumNames[i]
		}
	}
	return labels, f.Enum
}

// schemaFields reads the properties of an object schema in document order.
func schemaFields(schema json.RawMessage) ([]schemaField, error) {
	var top struct {
		Properties json.RawMessage `json:"properties"`
	}
	if len(schema) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(schema, &top); err != nil {
		return nil, err
	}
	if len(top.Properties) == 0 || string(top.Properties) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(top.Properties))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("properties is not an object")
	}
	var fields []schemaField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var f schemaField
		if err := dec.Decode(&f); err != nil {
			return nil, err
		}
		f.name, _ = tok.(string)
		fields = append(fields, f)
	}
	return fields, nil
}

// Elicit asks the user for the input a tool server requested, turning the
// requested schema into dialogs: enumerations become selects (shown as one
// questionnaire when there are two to five), booleans confirmations, other
// fields text input. It returns "accept" with the answers, "decline" when the
// user refuses a plain confirmation, or "cancel" when a dialog is dismissed.
// Elicit 把工具服务器请求的输入模式转成对话框：枚举为选择（2–5 个时合成一份问卷），
// 布尔为确认，其余为文本输入；返回 accept 及答案、decline 或 cancel。
func (s *Server) Elicit(ctx context.Context, source, message string, schema json.RawMessage) (string, map[string]any, error) {
	fields, err := schemaFields(schema)
	if err != nil {
		return "cancel", nil, err
	}
	if message == "" {
		message = source + " needs your input"
	}
	if len(fields) == 0 {
		answer, err := s.Ask(ctx, map[string]any{"method": "confirm", "title": message, "message": message})
		if err != nil || answer.Cancelled {
			return "cancel", nil, err
		}
		if !answer.Confirmed {
			return "decline", nil, nil
		}
		return "accept", map[string]any{}, nil
	}
	content := map[string]any{}
	if group := enumGroup(fields); group {
		return s.askQuestionnaire(ctx, message, fields)
	}
	for _, f := range fields {
		title := message
		if len(fields) > 1 {
			title = message + "\n\n" + f.label()
		}
		value, ok, err := s.askField(ctx, title, f)
		if err != nil || !ok {
			return "cancel", nil, err
		}
		content[f.name] = value
	}
	return "accept", content, nil
}

func enumGroup(fields []schemaField) bool {
	if len(fields) < 2 || len(fields) > 5 {
		return false
	}
	for _, f := range fields {
		if labels, _ := f.choices(); len(labels) == 0 {
			return false
		}
	}
	return true
}

func (s *Server) askQuestionnaire(ctx context.Context, message string, fields []schemaField) (string, map[string]any, error) {
	group := randomHex(6)
	type reply struct {
		index  int
		answer UIAnswer
		err    error
	}
	replies := make(chan reply, len(fields))
	for i, f := range fields {
		labels, _ := f.choices()
		options := append([]string{fmt.Sprintf("%s%s:%d:%d", questionnaireMarker, group, i, len(fields))}, labels...)
		go func(i int, title string, options []string) {
			answer, err := s.Ask(ctx, map[string]any{"method": "select", "title": title, "options": options})
			replies <- reply{i, answer, err}
		}(i, message+"\n\n"+f.label(), options)
	}
	content := map[string]any{}
	cancelled := false
	var firstErr error
	for range fields {
		r := <-replies
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		if r.err != nil || r.answer.Cancelled {
			cancelled = true
			continue
		}
		labels, values := fields[r.index].choices()
		content[fields[r.index].name] = choiceValue(labels, values, r.answer.Value)
	}
	if cancelled {
		return "cancel", nil, firstErr
	}
	return "accept", content, nil
}

// choiceValue maps a selected label back to its value; free-form answers
// pass through as text.
func choiceValue(labels []string, values []any, selected string) any {
	for i, l := range labels {
		if l == selected {
			return values[i]
		}
	}
	return selected
}

func (s *Server) askField(ctx context.Context, title string, f schemaField) (any, bool, error) {
	if labels, values := f.choices(); len(labels) > 0 {
		answer, err := s.Ask(ctx, map[string]any{"method": "select", "title": title, "options": labels})
		if err != nil || answer.Cancelled {
			return nil, false, err
		}
		return choiceValue(labels, values, answer.Value), true, nil
	}
	switch f.Type {
	case "boolean":
		message := f.Description
		if message == "" {
			message = f.label()
		}
		answer, err := s.Ask(ctx, map[string]any{"method": "confirm", "title": title, "message": message})
		if err != nil || answer.Cancelled {
			return nil, false, err
		}
		return answer.Confirmed, true, nil
	default:
		request := map[string]any{"method": "input", "title": title}
		if f.Description != "" {
			request["placeholder"] = f.Description
		}
		answer, err := s.Ask(ctx, request)
		if err != nil || answer.Cancelled {
			return nil, false, err
		}
		text := strings.TrimSpace(answer.Value)
		switch f.Type {
		case "integer":
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, false, nil
			}
			return n, true, nil
		case "number":
			n, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, false, nil
			}
			return n, true, nil
		}
		return answer.Value, true, nil
	}
}

// MultiDispatcher offers the tools of several dispatchers as one; when two
// offer the same name, the earlier one wins.
type MultiDispatcher struct {
	inner  []contract.ToolDispatcher
	mu     sync.Mutex
	routes map[string]contract.ToolDispatcher
}

// NewMultiDispatcher combines dispatchers, skipping nil ones.
func NewMultiDispatcher(inner ...contract.ToolDispatcher) *MultiDispatcher {
	m := &MultiDispatcher{routes: map[string]contract.ToolDispatcher{}}
	for _, d := range inner {
		if d != nil {
			m.inner = append(m.inner, d)
		}
	}
	return m
}

// ListTools lists every dispatcher's tools.
func (m *MultiDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	routes := map[string]contract.ToolDispatcher{}
	var defs []contract.ToolDef
	for _, inner := range m.inner {
		list, err := inner.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		for _, def := range list {
			if _, taken := routes[def.Name]; taken {
				continue
			}
			routes[def.Name] = inner
			defs = append(defs, def)
		}
	}
	m.mu.Lock()
	m.routes = routes
	m.mu.Unlock()
	return defs, nil
}

// Dispatch routes a call to the dispatcher that listed its name.
func (m *MultiDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	m.mu.Lock()
	inner := m.routes[call.Name]
	m.mu.Unlock()
	if inner == nil {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Tool %s is not available.", call.Name), IsError: true}, nil
	}
	return inner.Dispatch(ctx, call)
}
