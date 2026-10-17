package localtools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
)

func userHome() (string, error) { return os.UserHomeDir() }

func init() {
	register("read",
		fmt.Sprintf("Read the contents of a file. Supports text files and images (png, jpg, gif, webp); images are returned as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.", MaxLines, MaxBytes/1024),
		true,
		`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to read (relative or absolute)"},"offset":{"type":"number","description":"Line number to start reading from (1-indexed)"},"limit":{"type":"number","description":"Maximum number of lines to read"}},"required":["path"]}`,
		runRead)
	readTool := definitions["read"]
	readTool.media = readImage
	definitions["read"] = readTool
	register("write",
		"Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
		false,
		`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to write (relative or absolute)"},"content":{"type":"string","description":"Content to write to the file"}},"required":["path","content"]}`,
		runWrite)
	register("edit",
		"Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		false,
		`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},"edits":{"type":"array","description":"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits.","items":{"type":"object","properties":{"oldText":{"type":"string","description":"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},"newText":{"type":"string","description":"Replacement text for this targeted edit."}},"required":["oldText","newText"]}}},"required":["path","edits"]}`,
		runEdit)
	register("ls",
		fmt.Sprintf("List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).", DefaultLsMax, MaxBytes/1024),
		true,
		`{"type":"object","properties":{"path":{"type":"string","description":"Directory to list (default: current directory)"},"limit":{"type":"number","description":"Maximum number of entries to return (default: 500)"}}}`,
		runLs)
}

// MaxImageBytes bounds an image returned by read.
const MaxImageBytes = 20 << 20

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// readImage answers read for PNG, JPEG, GIF and WebP files with the image as
// a part, so models that accept images can see it.
func readImage(t *Tools, raw json.RawMessage) (string, []contract.ContentPart, bool, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(raw, &args); err != nil || args.Path == "" {
		return "", nil, false, nil
	}
	mediaType, ok := imageTypes[strings.ToLower(filepath.Ext(args.Path))]
	if !ok {
		return "", nil, false, nil
	}
	path := t.resolve(args.Path)
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, false, fmt.Errorf("Could not read %s: %v", args.Path, err)
	}
	if info.Size() > MaxImageBytes {
		return "", nil, false, fmt.Errorf("%s is larger than %d MB", args.Path, MaxImageBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, false, fmt.Errorf("Could not read %s: %v", args.Path, err)
	}
	text := fmt.Sprintf("Read image file %s [%s, %d bytes]", args.Path, mediaType, len(data))
	return text, []contract.ContentPart{{Type: "text", Text: text}, {Type: "image", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}}, true, nil
}

func runRead(_ context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Path   string  `json:"path"`
		Offset float64 `json:"offset"`
		Limit  float64 `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	path := t.resolve(args.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Could not read %s: %v", args.Path, err)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file and cannot be read as text", args.Path)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	start := 1
	if args.Offset >= 1 {
		start = int(args.Offset)
	}
	if start > total && total > 0 {
		return "", fmt.Errorf("Offset %d is beyond the end of the file (%d lines)", start, total)
	}
	maxLines := MaxLines
	if args.Limit >= 1 && int(args.Limit) < maxLines {
		maxLines = int(args.Limit)
	}
	selected := ""
	if total > 0 {
		selected = strings.Join(lines[start-1:], "")
	}
	text, truncated := truncateHead(selected, maxLines)
	shown := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") && text != "" {
		shown++
	}
	end := start + shown - 1
	if truncated || end < total {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += fmt.Sprintf("\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", start, end, total, end+1)
	}
	return text, nil
}

func runWrite(_ context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" || args.Content == nil {
		return "", errors.New("path and content are required")
	}
	path := t.resolve(args.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("Could not create the directory for %s: %v", args.Path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(*args.Content), mode); err != nil {
		return "", fmt.Errorf("Could not write %s: %v", args.Path, err)
	}
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(*args.Content), args.Path), nil
}

type replacement struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

func runEdit(_ context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Path  string        `json:"path"`
		Edits []replacement `json:"edits"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" || len(args.Edits) == 0 {
		return "", errors.New("path and at least one edit are required")
	}
	path := t.resolve(args.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Could not read %s: %v", args.Path, err)
	}
	original := string(data)
	// Files with CRLF line endings are matched with LF text and written back with CRLF.
	crlf := strings.Contains(original, "\r\n")
	if crlf {
		original = strings.ReplaceAll(original, "\r\n", "\n")
	}
	type span struct{ start, end, index int }
	spans := make([]span, 0, len(args.Edits))
	for i, e := range args.Edits {
		old := strings.ReplaceAll(e.OldText, "\r\n", "\n")
		if old == "" {
			return "", fmt.Errorf("edits[%d].oldText is empty", i)
		}
		count := strings.Count(original, old)
		switch {
		case count == 0:
			return "", fmt.Errorf("edits[%d].oldText was not found in %s. It must match the file exactly, including whitespace and indentation.", i, args.Path)
		case count > 1:
			return "", fmt.Errorf("edits[%d].oldText matches %d places in %s. Include more surrounding text so it is unique.", i, count, args.Path)
		}
		start := strings.Index(original, old)
		spans = append(spans, span{start, start + len(old), i})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", fmt.Errorf("edits[%d] and edits[%d] overlap. Merge them into one edit.", spans[i-1].index, spans[i].index)
		}
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(original[last:s.start])
		b.WriteString(strings.ReplaceAll(args.Edits[s.index].NewText, "\r\n", "\n"))
		last = s.end
	}
	b.WriteString(original[last:])
	updated := b.String()
	if crlf {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("Could not write %s: %v", args.Path, err)
	}
	return fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(args.Edits), args.Path), nil
}

func runLs(_ context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Path  string  `json:"path"`
		Limit float64 `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	limit := DefaultLsMax
	if args.Limit >= 1 {
		limit = int(args.Limit)
	}
	dir := t.resolve(args.Path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("Could not list %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	more := 0
	if len(names) > limit {
		more = len(names) - limit
		names = names[:limit]
	}
	text, truncated := truncateHead(strings.Join(names, "\n"), limit)
	if more > 0 || truncated {
		text += fmt.Sprintf("\n\n[%d more entries not shown. Use a larger limit to see them.]", more)
	}
	return text, nil
}
