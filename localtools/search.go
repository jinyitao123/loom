package localtools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxSearchFile skips files larger than this when searching contents.
const maxSearchFile = 10 << 20

func init() {
	register("grep",
		fmt.Sprintf("Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to %d matches or %dKB (whichever is hit first). Long lines are truncated to %d chars.", DefaultGrep, MaxBytes/1024, MaxGrepLine),
		true,
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Search pattern (regex or literal string)"},"path":{"type":"string","description":"Directory or file to search (default: current directory)"},"glob":{"type":"string","description":"Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"},"ignoreCase":{"type":"boolean","description":"Case-insensitive search (default: false)"},"literal":{"type":"boolean","description":"Treat pattern as literal string instead of regex (default: false)"},"context":{"type":"number","description":"Number of lines to show before and after each match (default: 0)"},"limit":{"type":"number","description":"Maximum number of matches to return (default: 100)"}},"required":["pattern"]}`,
		runGrep)
	register("find",
		fmt.Sprintf("Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to %d results or %dKB (whichever is hit first).", DefaultFindMx, MaxBytes/1024),
		true,
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"},"path":{"type":"string","description":"Directory to search in (default: current directory)"},"limit":{"type":"number","description":"Maximum number of results (default: 1000)"}},"required":["pattern"]}`,
		runFind)
}

// walk visits files under root (or root itself when it is a file), skipping
// directories and files ignored by gitignore rules (see ignoreMatcher). fn
// returns false to stop.
func walk(ctx context.Context, root string, fn func(abs, rel string) bool) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		fn(root, filepath.Base(root))
		return nil
	}
	rules := newIgnoreMatcher(root)
	rules.enter(root, "")
	stop := errors.New("stop")
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || abs == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(abs, root+string(filepath.Separator)))
		if rules.ignored(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			rules.enter(root, rel)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !fn(abs, rel) {
			return stop
		}
		return nil
	})
	if errors.Is(err, stop) {
		return nil
	}
	return err
}

// matchGlob matches a slash-separated relative path against a glob where
// "**" spans any number of directories. A pattern without "/" matches the
// file name at any depth.
func matchGlob(pattern, rel string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(rel))
		return ok
	}
	return matchSegments(strings.Split(strings.TrimPrefix(pattern, "/"), "/"), strings.Split(rel, "/"))
}

func matchSegments(pattern, parts []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(parts); i++ {
				if matchSegments(pattern[1:], parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], parts[0]); !ok {
			return false
		}
		pattern, parts = pattern[1:], parts[1:]
	}
	return len(parts) == 0
}

func runGrep(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Pattern    string  `json:"pattern"`
		Path       string  `json:"path"`
		Glob       string  `json:"glob"`
		IgnoreCase bool    `json:"ignoreCase"`
		Literal    bool    `json:"literal"`
		Context    float64 `json:"context"`
		Limit      float64 `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	expr := args.Pattern
	if args.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	if args.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("Invalid pattern: %v", err)
	}
	limit := DefaultGrep
	if args.Limit >= 1 {
		limit = int(args.Limit)
	}
	around := 0
	if args.Context > 0 {
		around = int(args.Context)
	}
	root := t.resolve(args.Path)
	var out []string
	matches := 0
	walkErr := walk(ctx, root, func(abs, rel string) bool {
		if args.Glob != "" && !matchGlob(args.Glob, rel) {
			return true
		}
		lines, ok := textLines(abs)
		if !ok {
			return true
		}
		lastPrinted := -1
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			from, to := max(0, i-around), min(len(lines)-1, i+around)
			if lastPrinted >= 0 && from > lastPrinted+1 {
				out = append(out, "--")
			}
			for j := max(from, lastPrinted+1); j <= to; j++ {
				sep := "-"
				if re.MatchString(lines[j]) {
					sep = ":"
				}
				out = append(out, fmt.Sprintf("%s%s%d%s %s", rel, sep, j+1, sep, clip(lines[j])))
			}
			lastPrinted = to
			matches++
			if matches >= limit {
				return false
			}
		}
		return true
	})
	if walkErr != nil {
		return "", fmt.Errorf("Could not search %s: %v", root, walkErr)
	}
	if matches == 0 {
		return "No matches found", nil
	}
	text, truncated := truncateHead(strings.Join(out, "\n"), MaxLines)
	if matches >= limit || truncated {
		text += fmt.Sprintf("\n\n[Results limited to %d matches. Narrow the search or raise the limit.]", limit)
	}
	return text, nil
}

func clip(line string) string {
	if len(line) <= MaxGrepLine {
		return line
	}
	cut := MaxGrepLine
	for cut > 0 && !utf8Start(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// textLines reads a text file into lines; binary and oversized files are skipped.
func textLines(abs string) ([]string, bool) {
	info, err := os.Stat(abs)
	if err != nil || info.Size() > maxSearchFile {
		return nil, false
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, false
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, false
	}
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxSearchFile)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	return lines, true
}

func runFind(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
	var args struct {
		Pattern string  `json:"pattern"`
		Path    string  `json:"path"`
		Limit   float64 `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	if args.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	limit := DefaultFindMx
	if args.Limit >= 1 {
		limit = int(args.Limit)
	}
	root := t.resolve(args.Path)
	var found []string
	more := false
	walkErr := walk(ctx, root, func(_, rel string) bool {
		if !matchGlob(args.Pattern, rel) {
			return true
		}
		if len(found) >= limit {
			more = true
			return false
		}
		found = append(found, rel)
		return true
	})
	if walkErr != nil {
		return "", fmt.Errorf("Could not search %s: %v", root, walkErr)
	}
	if len(found) == 0 {
		return "No files found matching pattern", nil
	}
	sort.Strings(found)
	text, truncated := truncateHead(strings.Join(found, "\n"), limit)
	if more || truncated {
		text += fmt.Sprintf("\n\n[Results limited to %d paths. Narrow the pattern or raise the limit.]", limit)
	}
	return text, nil
}
