package interactive

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionVersion is the session file format version this package writes.
const SessionVersion = 3

// maxSessionLine bounds one JSONL line when reading a session file.
const maxSessionLine = 64 << 20

// Header is the first line of a session file; it is not part of the entry tree.
type Header struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

// Entry is one node of the session tree. Entries this package does not create
// are kept byte-for-byte in raw and only interpreted where context needs them.
// Entry 是会话树中的一个节点；本包不创建的条目按原始字节保留，仅在构建上下文时解读。
type Entry struct {
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	ParentID         *string   `json:"parentId"`
	Timestamp        string    `json:"timestamp"`
	Message          *Message  `json:"message,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	ModelID          string    `json:"modelId,omitempty"`
	ThinkingLevel    string    `json:"thinkingLevel,omitempty"`
	Name             *string   `json:"name,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	FirstKeptEntryID string    `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int       `json:"tokensBefore,omitempty"`
	RetainedTail     []Message `json:"retainedTail,omitempty"`
	FromID           string    `json:"fromId,omitempty"`
	CustomType       string    `json:"customType,omitempty"`

	raw json.RawMessage
}

// Session is an append-only tree of entries persisted as JSONL. Like the
// format it follows, the file is created only once the first assistant
// message exists, so abandoned prompts leave no empty sessions behind.
// Session 是以 JSONL 持久化的只追加条目树；与所遵循的格式一致，
// 首条助手消息出现后才创建文件，避免留下空会话。
type Session struct {
	header  Header
	path    string
	entries []*Entry
	byID    map[string]*Entry
	leaf    string
	flushed bool
}

// SessionDir returns the default directory for sessions of a working
// directory: <root>/--<cwd with separators replaced by dashes>--.
// SessionDir 返回某工作目录的默认会话目录，命名规则与所遵循的格式一致。
func SessionDir(root, cwd string) string {
	trimmed := strings.TrimLeft(cwd, `/\`)
	safe := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(trimmed)
	return filepath.Join(root, "--"+safe+"--")
}

// NewSession starts an unsaved session in dir for cwd.
func NewSession(dir, cwd, parentSession string) *Session {
	now := time.Now().UTC()
	id := newUUID()
	stamp := now.Format("2006-01-02T15:04:05.000Z")
	return &Session{
		header: Header{Type: "session", Version: SessionVersion, ID: id, Timestamp: stamp, Cwd: cwd, ParentSession: parentSession},
		path:   filepath.Join(dir, strings.NewReplacer(":", "-", ".", "-").Replace(stamp)+"_"+id+".jsonl"),
		byID:   map[string]*Entry{},
	}
}

// OpenSession loads an existing session file. The last entry is the leaf.
func OpenSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Session{path: path, byID: map[string]*Entry{}, flushed: true}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxSessionLine)
	first := true
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal(line, &s.header); err != nil || s.header.Type != "session" {
				return nil, fmt.Errorf("interactive: %s is not a session file", path)
			}
			continue
		}
		entry := &Entry{}
		if err := json.Unmarshal(line, entry); err != nil {
			return nil, fmt.Errorf("interactive: %s: %w", path, err)
		}
		entry.raw = append(json.RawMessage(nil), line...)
		s.add(entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("interactive: read %s: %w", path, err)
	}
	if first {
		return nil, fmt.Errorf("interactive: %s is empty", path)
	}
	return s, nil
}

func (s *Session) add(entry *Entry) {
	s.entries = append(s.entries, entry)
	s.byID[entry.ID] = entry
	s.leaf = entry.ID
}

// Path returns the session file path (which may not exist yet).
func (s *Session) Path() string { return s.path }

// ID returns the session id.
func (s *Session) ID() string { return s.header.ID }

// Cwd returns the working directory recorded in the header.
func (s *Session) Cwd() string { return s.header.Cwd }

// Entries returns all entries in append order.
func (s *Session) Entries() []*Entry { return s.entries }

// Append adds an entry under the current leaf and persists it.
func (s *Session) Append(entry *Entry) error {
	entry.ID = s.newEntryID()
	if s.leaf != "" {
		parent := s.leaf
		entry.ParentID = &parent
	}
	entry.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	entry.raw = line
	s.add(entry)
	return s.persist()
}

func (s *Session) persist() error {
	if !s.flushed {
		if !s.hasAssistant() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return err
		}
		var buf bytes.Buffer
		header, err := json.Marshal(s.header)
		if err != nil {
			return err
		}
		buf.Write(header)
		buf.WriteByte('\n')
		for _, entry := range s.entries {
			buf.Write(entry.raw)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(s.path, buf.Bytes(), 0o600); err != nil {
			return err
		}
		s.flushed = true
		return nil
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	last := s.entries[len(s.entries)-1]
	_, werr := f.Write(append(append([]byte(nil), last.raw...), '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func (s *Session) hasAssistant() bool {
	for _, entry := range s.entries {
		if entry.Type == "message" && entry.Message != nil && entry.Message.Role == "assistant" {
			return true
		}
	}
	return false
}

func (s *Session) newEntryID() string {
	for {
		id := randomHex(4)
		if _, taken := s.byID[id]; !taken {
			return id
		}
	}
}

// Name returns the latest session display name, or "".
func (s *Session) Name() string {
	for i := len(s.entries) - 1; i >= 0; i-- {
		if e := s.entries[i]; e.Type == "session_info" && e.Name != nil {
			return *e.Name
		}
	}
	return ""
}

// branch returns the entries from the root to the current leaf.
func (s *Session) branch() []*Entry {
	var path []*Entry
	for id := s.leaf; id != ""; {
		entry, ok := s.byID[id]
		if !ok {
			break
		}
		path = append(path, entry)
		if entry.ParentID == nil {
			break
		}
		id = *entry.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Context is the active conversation of a session.
type Context struct {
	Messages      []Message
	Provider      string
	ModelID       string
	ThinkingLevel string
}

// Context builds the active conversation from the root to the leaf, honoring
// the latest compaction on the path.
// Context 从根到叶构建当前对话，并按路径上最近一次压缩截取。
func (s *Session) Context() Context {
	path := s.branch()
	var ctx Context
	for _, entry := range path {
		switch entry.Type {
		case "model_change":
			ctx.Provider, ctx.ModelID = entry.Provider, entry.ModelID
		case "thinking_level_change":
			ctx.ThinkingLevel = entry.ThinkingLevel
		case "message":
			if m := entry.Message; m != nil && m.Role == "assistant" && m.Provider != "" && ctx.ModelID == "" {
				ctx.Provider, ctx.ModelID = m.Provider, m.Model
			}
		}
	}

	start := 0
	compaction := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Type == "compaction" {
			compaction = i
			break
		}
	}
	if compaction >= 0 {
		c := path[compaction]
		ctx.Messages = append(ctx.Messages, Message{Role: "compactionSummary", Summary: c.Summary, TokensBefore: c.TokensBefore})
		if len(c.RetainedTail) > 0 {
			ctx.Messages = append(ctx.Messages, c.RetainedTail...)
		} else if c.FirstKeptEntryID != "" {
			for i := 0; i < compaction; i++ {
				if path[i].ID == c.FirstKeptEntryID {
					for _, kept := range path[i:compaction] {
						ctx.Messages = appendEntryMessage(ctx.Messages, kept)
					}
					break
				}
			}
		}
		start = compaction + 1
	}
	for _, entry := range path[start:] {
		ctx.Messages = appendEntryMessage(ctx.Messages, entry)
	}
	return ctx
}

func appendEntryMessage(messages []Message, entry *Entry) []Message {
	switch entry.Type {
	case "message":
		if entry.Message != nil {
			return append(messages, *entry.Message)
		}
	case "custom_message":
		var m Message
		if json.Unmarshal(entry.raw, &m) == nil {
			m.Role = "custom"
			return append(messages, m)
		}
	case "branch_summary":
		return append(messages, Message{Role: "branchSummary", Summary: entry.Summary, FromID: entry.FromID})
	}
	return messages
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
