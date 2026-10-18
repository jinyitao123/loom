// Package localtools is a contract.ToolDispatcher for working in a local
// directory: read, write and edit files, run shell commands, list, search and
// find files. Tool names, parameters and output limits follow common coding
// agent conventions so models use them without extra prompting. It is a leaf
// adapter, like the providers: it implements contract interfaces and imports
// nothing above contract.
//
// localtools 是在本地目录中工作的 contract.ToolDispatcher：读写与编辑文件、
// 执行命令、列目录、搜索内容与查找文件。工具名、参数与输出上限沿用常见编码智能体的约定。
// 它与 provider 一样是叶子适配器，只实现 contract 接口。
package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// Output limits shared by the tools.
const (
	MaxLines      = 2000      // lines returned by read and kept by bash
	MaxBytes      = 50 * 1024 // bytes returned by any tool
	MaxGrepLine   = 500       // characters kept per grep match line
	DefaultLsMax  = 500       // entries returned by ls
	DefaultGrep   = 100       // matches returned by grep
	DefaultFindMx = 1000      // paths returned by find
)

// Default is the tool set enabled when none is named.
var Default = []string{"read", "bash", "edit", "write"}

// All lists every available tool.
var All = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}

// Tools dispatches the local tools rooted at a working directory.
type Tools struct {
	cwd      string
	enabled  []string
	progress func(callID, output string)
}

// OnProgress receives the output so far of a running bash call, at most every
// ProgressInterval, keyed by the tool call id. Set it before dispatching.
// OnProgress 接收运行中 bash 调用的阶段输出（迄今输出，按工具调用 ID），
// 至多每 ProgressInterval 一次；须在派发前设置。
func (t *Tools) OnProgress(fn func(callID, output string)) { t.progress = fn }

// ProgressInterval is the minimum gap between progress reports of one call.
var ProgressInterval = 250 * time.Millisecond

type callIDKey struct{}

// report sends the output so far of the call running in ctx.
func (t *Tools) report(ctx context.Context, output string) {
	if id, _ := ctx.Value(callIDKey{}).(string); t.progress != nil && id != "" {
		t.progress(id, output)
	}
}

// New returns the named tools working in cwd; no names selects Default.
func New(cwd string, names ...string) (*Tools, error) {
	if len(names) == 0 {
		names = Default
	}
	for _, name := range names {
		if _, ok := definitions[name]; !ok {
			return nil, fmt.Errorf("localtools: unknown tool %q", name)
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	return &Tools{cwd: abs, enabled: append([]string(nil), names...)}, nil
}

// ListTools returns the enabled tool definitions.
func (t *Tools) ListTools(context.Context) ([]contract.ToolDef, error) {
	defs := make([]contract.ToolDef, 0, len(t.enabled))
	for _, name := range t.enabled {
		defs = append(defs, definitions[name].def)
	}
	return defs, nil
}

// Dispatch runs one tool call. Failures the model can act on (missing file,
// no match, non-zero exit) come back as IsError results, not Go errors.
// Dispatch 执行一次工具调用；模型可据以调整的失败（文件不存在、未匹配、非零退出）以 IsError 结果返回。
func (t *Tools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tool, ok := definitions[call.Name]
	if !ok || !t.isEnabled(call.Name) {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Tool %s is not available.", call.Name), IsError: true}, nil
	}
	args := strings.TrimSpace(call.Args)
	if args == "" {
		args = "{}"
	}
	if tool.media != nil {
		text, parts, handled, err := tool.media(t, json.RawMessage(args))
		if err != nil {
			return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil
		}
		if handled {
			return &contract.ToolResult{CallID: call.ID, Content: text, Parts: parts}, nil
		}
	}
	content, err := tool.run(context.WithValue(ctx, callIDKey{}, call.ID), t, json.RawMessage(args))
	if err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil
	}
	return &contract.ToolResult{CallID: call.ID, Content: content}, nil
}

func (t *Tools) isEnabled(name string) bool {
	for _, n := range t.enabled {
		if n == name {
			return true
		}
	}
	return false
}

// resolve maps a tool path argument to an absolute path; relative paths are
// taken from the working directory and a leading ~ from the home directory.
func (t *Tools) resolve(path string) string {
	if path == "" {
		return t.cwd
	}
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := userHome(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.cwd, path)
	}
	return filepath.Clean(path)
}

type toolSpec struct {
	def contract.ToolDef
	run func(ctx context.Context, t *Tools, args json.RawMessage) (string, error)
	// media, when set, may answer a call with text and image parts instead of run.
	media func(t *Tools, args json.RawMessage) (text string, parts []contract.ContentPart, handled bool, err error)
}

var definitions = map[string]toolSpec{}

func register(name, description string, readOnly bool, schema string, run func(context.Context, *Tools, json.RawMessage) (string, error)) {
	definitions[name] = toolSpec{
		def: contract.ToolDef{Name: name, Description: description, InputSchema: json.RawMessage(schema), ReadOnly: readOnly},
		run: run,
	}
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("Invalid arguments: %v", err)
	}
	return nil
}

// truncateHead keeps the first MaxLines lines and MaxBytes bytes.
func truncateHead(text string, maxLines int) (string, bool) {
	lines := strings.SplitAfter(text, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	var b strings.Builder
	for _, line := range lines {
		if b.Len()+len(line) > MaxBytes {
			truncated = true
			break
		}
		b.WriteString(line)
	}
	return b.String(), truncated
}

// truncateTail keeps the last MaxLines lines and MaxBytes bytes.
func truncateTail(text string) (string, bool) {
	lines := strings.SplitAfter(text, "\n")
	truncated := false
	if len(lines) > MaxLines {
		lines = lines[len(lines)-MaxLines:]
		truncated = true
	}
	size := 0
	start := len(lines)
	for start > 0 && size+len(lines[start-1]) <= MaxBytes {
		start--
		size += len(lines[start])
	}
	if start > 0 {
		truncated = true
	}
	return strings.Join(lines[start:], ""), truncated
}
