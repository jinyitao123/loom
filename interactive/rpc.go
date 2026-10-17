package interactive

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Server speaks the JSONL command/event protocol: one command per input line,
// one response or event per output line. Lines are delimited by LF only; a
// trailing CR is stripped.
// Server 实现 JSONL 命令/事件协议：每行输入一条命令，每行输出一条响应或事件；
// 只以 LF 分行，并去掉行尾 CR。
type Server struct {
	agent *Agent
	mu    sync.Mutex
	out   io.Writer
	uiState
}

// NewServer returns a server writing to out. Call Attach before Serve.
func NewServer(out io.Writer) *Server { return &Server{out: out} }

// Attach binds the agent whose events the server's Emit receives.
func (s *Server) Attach(agent *Agent) { s.agent = agent }

// Emit writes one event line. It is safe for concurrent use.
func (s *Server) Emit(event any) {
	line, err := json.Marshal(event)
	if err != nil {
		line, _ = json.Marshal(map[string]any{"type": "extension_error", "extensionPath": "protocol", "event": "encode", "error": err.Error()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(line, '\n'))
}

type command struct {
	ID                 string  `json:"id,omitempty"`
	Type               string  `json:"type"`
	Message            string  `json:"message"`
	Images             []Block `json:"images"`
	StreamingBehavior  string  `json:"streamingBehavior"`
	ParentSession      string  `json:"parentSession"`
	SessionPath        string  `json:"sessionPath"`
	Provider           string  `json:"provider"`
	ModelID            string  `json:"modelId"`
	Level              string  `json:"level"`
	Mode               string  `json:"mode"`
	Enabled            bool    `json:"enabled"`
	Name               string  `json:"name"`
	CustomInstructions string  `json:"customInstructions"`
	EntryID            string  `json:"entryId"`
	Value              *string `json:"value"`
	Confirmed          *bool   `json:"confirmed"`
	Cancelled          bool    `json:"cancelled"`
}

// Serve reads commands until in is exhausted, then aborts any running prompt.
func (s *Server) Serve(in io.Reader) error {
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := strings.TrimSpace(strings.TrimSuffix(string(line), "\r")); trimmed != "" {
			s.handle([]byte(trimmed))
		}
		if errors.Is(err, io.EOF) {
			s.agent.Abort()
			return nil
		}
		if err != nil {
			s.agent.Abort()
			return err
		}
	}
}

func (s *Server) respond(cmd command, data any, err error) {
	response := map[string]any{"type": "response", "command": cmd.Type, "success": err == nil}
	if cmd.ID != "" {
		response["id"] = cmd.ID
	}
	if err != nil {
		response["error"] = err.Error()
	} else if data != nil {
		response["data"] = data
	}
	s.Emit(response)
}

func (s *Server) handle(line []byte) {
	var cmd command
	if err := json.Unmarshal(line, &cmd); err != nil {
		s.respond(command{Type: "parse"}, nil, fmt.Errorf("Failed to parse command: %v", err))
		return
	}
	a := s.agent
	switch cmd.Type {
	case "get_state":
		s.respond(cmd, a.State(), nil)
	case "prompt", "steer", "follow_up":
		images, err := validImages(cmd.Images)
		if err != nil {
			s.respond(cmd, nil, err)
			return
		}
		if strings.TrimSpace(cmd.Message) == "" && len(images) == 0 {
			s.respond(cmd, nil, errors.New("message is empty"))
			return
		}
		switch cmd.Type {
		case "prompt":
			err = a.Prompt(cmd.Message, images, cmd.StreamingBehavior)
		case "steer":
			err = a.Steer(cmd.Message, images)
		default:
			err = a.FollowUp(cmd.Message, images)
		}
		s.respond(cmd, nil, err)
	case "compact":
		// Compaction calls the model; answer from a goroutine so other
		// commands (get_state, abort) keep flowing meanwhile.
		go func() {
			result, err := a.Compact(cmd.CustomInstructions)
			s.respond(cmd, result, err)
		}()
	case "abort", "abort_retry":
		if cmd.Type == "abort" {
			a.Abort()
		}
		s.respond(cmd, nil, nil)
	case "new_session":
		a.NewSession(cmd.ParentSession)
		s.respond(cmd, map[string]any{"cancelled": false}, nil)
	case "switch_session":
		if err := a.SwitchSession(cmd.SessionPath); err != nil {
			s.respond(cmd, nil, err)
			return
		}
		s.respond(cmd, map[string]any{"cancelled": false}, nil)
	case "get_messages":
		s.respond(cmd, map[string]any{"messages": nonNil(a.Messages())}, nil)
	case "get_available_models":
		models := a.Models()
		if models == nil {
			models = []Model{}
		}
		s.respond(cmd, map[string]any{"models": models}, nil)
	case "set_model":
		m, err := a.SetModel(cmd.Provider, cmd.ModelID)
		s.respond(cmd, m, err)
	case "cycle_model":
		s.respond(cmd, s.cycleModel(), nil)
	case "set_thinking_level":
		s.respond(cmd, nil, a.SetThinkingLevel(cmd.Level))
	case "cycle_thinking_level":
		s.respond(cmd, s.cycleThinking(), nil)
	case "get_available_thinking_levels":
		s.respond(cmd, map[string]any{"levels": s.thinkingLevels()}, nil)
	case "set_steering_mode", "set_follow_up_mode":
		if cmd.Mode != "all" && cmd.Mode != "one-at-a-time" {
			s.respond(cmd, nil, fmt.Errorf("invalid mode %q", cmd.Mode))
			return
		}
		if cmd.Type == "set_steering_mode" {
			a.SetQueueModes(cmd.Mode, "")
		} else {
			a.SetQueueModes("", cmd.Mode)
		}
		s.respond(cmd, nil, nil)
	case "set_auto_compaction":
		a.SetAutoCompaction(cmd.Enabled)
		s.respond(cmd, nil, nil)
	case "set_auto_retry":
		a.SetAutoRetry(cmd.Enabled)
		s.respond(cmd, nil, nil)
	case "get_session_stats":
		s.respond(cmd, a.Stats(), nil)
	case "get_last_assistant_text":
		s.respond(cmd, map[string]any{"text": a.LastAssistantText()}, nil)
	case "set_session_name":
		s.respond(cmd, nil, a.SetSessionName(cmd.Name))
	case "get_fork_messages":
		messages := a.ForkMessages()
		if messages == nil {
			messages = []map[string]string{}
		}
		s.respond(cmd, map[string]any{"messages": messages}, nil)
	case "get_commands":
		commands := []map[string]string{}
		for _, skill := range a.opts.Skills {
			commands = append(commands, map[string]string{"name": "skill:" + skill.Name, "description": skill.Description, "source": "skill", "location": skill.Location, "path": skill.Path})
		}
		s.respond(cmd, map[string]any{"commands": commands}, nil)
	case "extension_ui_response":
		// Hosts send dialog answers without waiting for a reply.
		s.deliverUI(cmd)
	case "fork", "clone", "bash", "abort_bash", "get_entries", "get_tree", "export_html":
		s.respond(cmd, nil, fmt.Errorf("%s is not supported by the Loom kernel yet", cmd.Type))
	default:
		s.respond(cmd, nil, fmt.Errorf("Unknown command: %s", cmd.Type))
	}
}

// MaxImageBytes bounds one decoded image.
const MaxImageBytes = 20 << 20

// validImages checks prompt images: PNG, JPEG, GIF or WebP, canonical base64,
// at most 8 images of MaxImageBytes each.
func validImages(images []Block) ([]Block, error) {
	if len(images) > 8 {
		return nil, errors.New("at most 8 images per message")
	}
	out := make([]Block, 0, len(images))
	for i, img := range images {
		if img.Type != "image" {
			return nil, fmt.Errorf("images[%d].type must be image", i)
		}
		switch strings.ToLower(img.MimeType) {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return nil, fmt.Errorf("images[%d]: unsupported image type %q", i, img.MimeType)
		}
		data, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil || len(data) == 0 {
			return nil, fmt.Errorf("images[%d].data must be base64", i)
		}
		if len(data) > MaxImageBytes {
			return nil, fmt.Errorf("images[%d] is larger than %d MB", i, MaxImageBytes>>20)
		}
		out = append(out, Block{Type: "image", Data: img.Data, MimeType: strings.ToLower(img.MimeType)})
	}
	return out, nil
}

func (s *Server) cycleModel() any {
	models := s.agent.Models()
	if len(models) < 2 {
		return nil
	}
	state := s.agent.State()
	current, _ := state["model"].(*Model)
	next := models[0]
	for i, m := range models {
		if current != nil && m.Provider == current.Provider && m.ID == current.ID {
			next = models[(i+1)%len(models)]
			break
		}
	}
	m, err := s.agent.SetModel(next.Provider, next.ID)
	if err != nil {
		return nil
	}
	return map[string]any{"model": m, "thinkingLevel": state["thinkingLevel"], "isScoped": false}
}

func (s *Server) thinkingLevels() []string {
	current, _ := s.agent.State()["model"].(*Model)
	if current == nil || !current.Reasoning {
		return []string{"off"}
	}
	return []string{"off", "minimal", "low", "medium", "high"}
}

func (s *Server) cycleThinking() any {
	levels := s.thinkingLevels()
	if len(levels) < 2 {
		return nil
	}
	current, _ := s.agent.State()["thinkingLevel"].(string)
	next := levels[0]
	for i, l := range levels {
		if l == current {
			next = levels[(i+1)%len(levels)]
			break
		}
	}
	if err := s.agent.SetThinkingLevel(next); err != nil {
		return nil
	}
	return map[string]any{"level": next}
}
