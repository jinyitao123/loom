package deepseek_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/loom"
	"github.com/anthropic/loom/contract"
	"github.com/anthropic/loom/provider/deepseek"
	"github.com/anthropic/loom/stdlib"
)

func getAPIKey(t *testing.T) string {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set, skipping integration test")
	}
	return key
}

func TestDeepSeek_SimpleChat(t *testing.T) {
	key := getAPIKey(t)
	client := deepseek.New(key)

	resp, err := client.Chat(context.Background(), contract.ChatRequest{
		Model: "deepseek-chat",
		Messages: []contract.Message{
			{Role: "user", Content: "Reply with exactly: PONG"},
		},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	t.Logf("Response: %q (tokens: in=%d out=%d)", resp.Content, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	if resp.Content == "" {
		t.Error("empty response")
	}
}

func TestDeepSeek_ToolCall(t *testing.T) {
	key := getAPIKey(t)
	client := deepseek.New(key)

	resp, err := client.Chat(context.Background(), contract.ChatRequest{
		Model: "deepseek-chat",
		Messages: []contract.Message{
			{Role: "user", Content: "What is the weather in Beijing? Use the get_weather tool."},
		},
		Tools: []contract.ToolDef{{
			Name:        "get_weather",
			Description: "Get weather for a city",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
		MaxTokens: 200,
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}

	if len(resp.ToolCalls) == 0 {
		t.Fatalf("expected tool call, got content: %q", resp.Content)
	}
	tc := resp.ToolCalls[0]
	t.Logf("Tool call: %s(%s)", tc.Name, tc.Args)
	if tc.Name != "get_weather" {
		t.Errorf("tool name = %s, want get_weather", tc.Name)
	}
}

func TestDeepSeek_E2E_ToolLoop(t *testing.T) {
	key := getAPIKey(t)
	client := deepseek.New(key)

	tools := &calculatorTools{}

	g := loom.NewGraph("calc-agent", "chat")
	g.AddStep("chat", stdlib.NewToolLoopStep(client, tools, stdlib.ToolLoopOpts{
		Model:         "deepseek-chat",
		SystemPrompt:  "You are a calculator assistant. Use the add tool to add numbers. Be concise.",
		MaxIterations: 5,
	}), loom.End())

	result, err := g.Run(context.Background(), loom.State{
		"messages": []contract.Message{
			{Role: "user", Content: "What is 17 + 25? Use the add tool to compute it."},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	output, _ := result.State["output"].(string)
	t.Logf("Agent output: %s", output)
	if output == "" {
		t.Error("empty output")
	}
}

func TestDeepSeek_Stream(t *testing.T) {
	key := getAPIKey(t)
	client := deepseek.New(key)

	ch, err := client.Stream(context.Background(), contract.ChatRequest{
		Model: "deepseek-chat",
		Messages: []contract.Message{
			{Role: "user", Content: "Count from 1 to 5, separated by commas."},
		},
		MaxTokens: 30,
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}

	var fullContent string
	chunks := 0
	gotDone := false
	for chunk := range ch {
		if chunk.Content != "" {
			fullContent += chunk.Content
			chunks++
		}
		if chunk.Done {
			gotDone = true
		}
	}

	t.Logf("Stream: %d chunks, content: %q", chunks, fullContent)
	if !gotDone {
		t.Error("never received done signal")
	}
	if fullContent == "" {
		t.Error("empty stream content")
	}
	if chunks < 2 {
		t.Errorf("expected multiple chunks, got %d (streaming may not be working)", chunks)
	}
}

func TestDeepSeek_Enterprise_ParallelChat(t *testing.T) {
	key := getAPIKey(t)
	if os.Getenv("DEEPSEEK_ENTERPRISE_TEST") == "" {
		t.Skip("set DEEPSEEK_ENTERPRISE_TEST=1 to enable enterprise parallel validation")
	}

	client := deepseek.New(key)

	const (
		totalRequests = 40
		workers       = 8
		maxErrorRate  = 0.15
	)

	jobs := make(chan int, workers)
	var okCount atomic.Int64
	var errCount atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				resp, err := client.Chat(ctx, contract.ChatRequest{
					Model: "deepseek-chat",
					Messages: []contract.Message{{
						Role:    "user",
						Content: fmt.Sprintf("Reply with exactly: ENTERPRISE_OK_%d", id),
					}},
					MaxTokens: 20,
				})
				cancel()

				if err != nil {
					errCount.Add(1)
					continue
				}
				if resp == nil || resp.Content == "" {
					errCount.Add(1)
					continue
				}
				okCount.Add(1)
			}
		}()
	}

	for i := 0; i < totalRequests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	ok := okCount.Load()
	errN := errCount.Load()
	rate := float64(errN) / float64(totalRequests)

	t.Logf("parallel deepseek validation: total=%d ok=%d err=%d error_rate=%.2f", totalRequests, ok, errN, rate)
	if rate > maxErrorRate {
		t.Fatalf("error rate %.2f exceeds allowed %.2f", rate, maxErrorRate)
	}
}

func TestDeepSeek_Enterprise_ParallelToolLoopBusinessCalls(t *testing.T) {
	key := getAPIKey(t)
	if os.Getenv("DEEPSEEK_ENTERPRISE_TEST") == "" {
		t.Skip("set DEEPSEEK_ENTERPRISE_TEST=1 to enable enterprise parallel validation")
	}

	client := deepseek.New(key)
	tools := &calculatorTools{}

	g := loom.NewGraph("enterprise-biz-agent", "chat")
	g.AddStep("chat", stdlib.NewToolLoopStep(client, tools, stdlib.ToolLoopOpts{
		Model:         "deepseek-chat",
		SystemPrompt:  "You are a production calculator agent. Always use add tool for arithmetic. Reply concisely with RESULT:<number>.",
		MaxIterations: 6,
	}), loom.End())

	const (
		totalSessions = 30
		workers       = 6
		maxErrorRate  = 0.20
	)

	jobs := make(chan int, workers)
	var okCount atomic.Int64
	var errCount atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				a := id + 17
				b := id + 29
				want := strconv.Itoa(a + b)

				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				res, err := g.Run(ctx, loom.State{
					"messages": []contract.Message{{
						Role:    "user",
						Content: fmt.Sprintf("Business request #%d: compute %d + %d with the add tool and return RESULT:<number>.", id, a, b),
					}},
				}, nil)
				cancel()

				if err != nil || res == nil {
					errCount.Add(1)
					continue
				}
				output, _ := res.State["output"].(string)
				if output == "" || !strings.Contains(output, want) {
					errCount.Add(1)
					continue
				}
				okCount.Add(1)
			}
		}()
	}

	for i := 0; i < totalSessions; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	ok := okCount.Load()
	errN := errCount.Load()
	rate := float64(errN) / float64(totalSessions)

	t.Logf("parallel toolloop business validation: total=%d ok=%d err=%d error_rate=%.2f", totalSessions, ok, errN, rate)
	if rate > maxErrorRate {
		t.Fatalf("error rate %.2f exceeds allowed %.2f", rate, maxErrorRate)
	}
}

// calculatorTools provides a simple add tool for testing.
type calculatorTools struct{}

func (c *calculatorTools) ListTools(_ context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{
		Name:        "add",
		Description: "Add two numbers together",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
	}}, nil
}

func (c *calculatorTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var args struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: "invalid args: " + err.Error(), IsError: true}, nil
	}
	return &contract.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("%.0f", args.A+args.B),
	}, nil
}
