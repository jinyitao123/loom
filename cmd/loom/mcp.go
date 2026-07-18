// mcp.go —— MCP 工具服务器接入层。
// 读取 .loom-mcp.json → 为每台服务器建立 HTTP JSON-RPC 客户端（initialize 握手、
// tools/list 清单拉取、tools/call 转发）→ 聚合为引擎眼中的单一工具分发器。
// 加载策略：可选服务器失败弹性跳过（resilient tool loading）；
// 必需（required）服务器失败在引擎启动前的预检阶段即大声报错（fail loudly）。

package main

import (
	"bytes"         // JSON-RPC 请求体的内存缓冲与响应体裁剪
	"context"       // 每次 HTTP 调用的取消/超时传播
	"encoding/json" // JSON-RPC 报文与 MCP 工具结果的编解码
	"errors"        // 错误哨兵判断（文件不存在）与多错误合并
	"fmt"           // 带服务器名/方法名上下文的错误包装
	"io"            // HTTP 响应体的全量读取
	"io/fs"         // fs.ErrNotExist 哨兵：识别"配置缺失非错误"
	"log/slog"      // 结构化日志：工具加载成败的可观测性
	"net/http"      // HTTP JSON-RPC 传输层
	"os"            // 读取 .loom-mcp.json 配置文件
	"sync"          // initialize 握手的 Once 与路由表的互斥保护
	"time"          // 单次 HTTP 调用的超时上限

	"github.com/jinyitao123/loom/contract" // ToolDef/ToolCall/ToolDispatcher 契约类型
)

// .loom-mcp.json — written by the weave daemon (writeMcpConfigFiles, C.7) from
// the agent's mcpServers, read here from the agent workdir. v1 supports HTTP
// JSON-RPC MCP servers only (the same wire protocol as weave/internal/mcphost
// and the Spare-parts backend).
//
//	{ "mcpServers": { "<name>": { "url": "http://…", "type": "http",
//	                              "tools": ["allow1","allow2"], "required": true } } }
//
// mcpConfig 对应 .loom-mcp.json 的顶层结构：由 weave 守护进程按 agent 的 mcpServers
// 生成、loom 从 agent 工作目录读取。v1 只支持 HTTP JSON-RPC 形态的 MCP 服务器。
type mcpConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"` // 服务器名 → 连接配置
}

// mcpServerConfig 描述单个 MCP 服务器的接入方式与访问约束。
type mcpServerConfig struct {
	URL  string `json:"url"`  // 服务器的 HTTP JSON-RPC 端点（必填）
	Type string `json:"type"` // 传输类型；仅接受空值或 "http"，其余在加载期即拒绝
	// Tools 非空时充当工具白名单：清单与调用都只放行名单内的工具。
	Tools    []string `json:"tools"`    // optional allowlist
	Required bool     `json:"required"` // 为 true 表示硬依赖：该服务器加载失败将令整次预检失败（fail loudly）
}

// loadMCPDispatcher reads path and returns a tool dispatcher. A missing file is
// not an error — it yields noTools (the agent simply has no tools).
// loadMCPDispatcher 读取 MCP 配置并装配工具分发器。关键约定：文件缺失不是错误，
// 而是"该 agent 没有工具"的合法形态，返回 noTools 让纯对话照常进行。
func loadMCPDispatcher(path string) (contract.ToolDispatcher, error) {
	// #nosec G304 -- path is the operator/daemon-supplied .loom-mcp.json in the
	// agent workdir, not untrusted remote input.
	// 路径来自运维/守护进程而非远端输入，读取本身可信（上方为安全扫描的豁免标注）。
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) { // 文件不存在：降级为零工具，而非报错
		return noTools{}, nil
	}
	if err != nil { // 其余读取错误（权限等）如实上抛
		return nil, err
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil { // 配置存在但不合法：必须报错，绝不静默忽略坏配置
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var servers []*mcpHost
	for name, sc := range cfg.MCPServers { // 逐一校验并实例化每台服务器的客户端
		if sc.URL == "" { // url 是必填项：缺失说明配置生成方有 bug，立即失败
			return nil, fmt.Errorf("mcp server %q: missing url", name)
		}
		if sc.Type != "" && sc.Type != "http" { // v1 仅支持 http 传输，显式拒绝其他类型以防误配被静默吞掉
			return nil, fmt.Errorf("mcp server %q: unsupported type %q (v1 supports http)", name, sc.Type)
		}
		servers = append(servers, newMCPHost(name, sc.URL, sc.Tools, sc.Required))
	}
	if len(servers) == 0 { // 配置存在但服务器表为空：等价于无工具
		return noTools{}, nil
	}
	return &mcpDispatcher{servers: servers}, nil // 多台服务器聚合为单一分发器暴露给引擎
}

// mcpHost is a single-server JSON-RPC MCP client implementing the tools/list +
// tools/call methods, with an optional tool allowlist.
// mcpHost 是面向单台 MCP 服务器的 JSON-RPC 客户端：负责 initialize 握手、
// tools/list 清单拉取与 tools/call 转发，可叠加工具白名单过滤。
type mcpHost struct {
	name     string          // 配置中的服务器名，用于日志与错误定位
	baseURL  string          // JSON-RPC 端点地址
	client   *http.Client    // 复用的 HTTP 客户端（带统一超时）
	filter   map[string]bool // 工具白名单集合；nil 表示不过滤、全部放行
	required bool            // 硬依赖标记：加载失败时由聚合层升级为预检失败
	initOnce sync.Once       // 保证 initialize 握手在整个生命周期只执行一次
	initErr  error           // 握手结果缓存：失败后所有后续操作直接复用该错误
}

// newMCPHost 构造单服务器客户端，并把白名单切片物化为集合以便 O(1) 查询。
func newMCPHost(name, baseURL string, allow []string, required bool) *mcpHost {
	h := &mcpHost{
		name:     name,
		baseURL:  baseURL,
		client:   &http.Client{Timeout: 30 * time.Second}, // 单次调用 30 秒硬超时，防止慢服务器拖垮整轮 agent
		required: required,
	}
	if len(allow) > 0 { // 仅在配置了白名单时才建集合：nil filter 的语义是"全部放行"
		h.filter = make(map[string]bool, len(allow))
		for _, n := range allow {
			h.filter[n] = true
		}
	}
	return h
}

// jsonRPCRequest 是 JSON-RPC 2.0 请求报文；ID 为零值时被 omitempty 略去，
// 整个报文即成为无需应答的通知（notification）。
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`          // 协议版本，恒为 "2.0"
	Method  string `json:"method"`           // 方法名，如 initialize / tools/list / tools/call
	Params  any    `json:"params,omitempty"` // 方法参数，无参时整个键省略
	ID      int    `json:"id,omitempty"`     // 请求 id；缺省即通知语义
}

// jsonRPCResponse 是 JSON-RPC 2.0 应答报文：Result 与 Error 互斥出现。
type jsonRPCResponse struct {
	Result json.RawMessage `json:"result,omitempty"` // 成功载荷，保留原始 JSON 由调用方按方法各自解码
	Error  *struct {
		Code    int    `json:"code"`    // 协议错误码
		Message string `json:"message"` // 人类可读错误信息
	} `json:"error,omitempty"` // 失败载荷；非 nil 即视为调用失败
}

// call 发起一次带应答的 JSON-RPC 调用（固定 ID=1：同一客户端上请求串行发出），
// 统一处理编解码、HTTP 状态与协议级错误，返回原始 Result 交由调用方解码。
func (h *mcpHost) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1}) // 组装并序列化请求报文
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL, bytes.NewReader(data)) // 绑定 ctx：上层取消/超时能立刻中断在途请求
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json") // JSON-RPC over HTTP 的固定内容类型
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s call %s: %w", h.name, method, err) // 网络层失败：错误里带上服务器名与方法名便于排障
	}
	defer resp.Body.Close() // 确保响应体关闭，底层连接可复用
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // 非 2xx 一律视为失败，并附上响应体片段辅助定位
		return nil, fmt.Errorf("mcp %s call %s: http %d: %s", h.name, method, resp.StatusCode, bytes.TrimSpace(body))
	}
	var rpc jsonRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil { // 应答不是合法 JSON-RPC：按解析错误上抛
		return nil, fmt.Errorf("mcp %s response parse: %w", h.name, err)
	}
	if rpc.Error != nil { // HTTP 成功但协议层报错：翻译为 Go 错误
		return nil, fmt.Errorf("mcp %s error %d: %s", h.name, rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil // 只返回原始 Result，具体结构由各方法自行解码
}

// notify 发送 JSON-RPC 通知（不带 ID、不期待 Result），用于 initialize 握手的
// notifications/initialized 确认；成败仅以传输层与 HTTP 状态为准。
func (h *mcpHost) notify(ctx context.Context, method string, params any) error {
	data, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}) // 省略 ID 字段即通知语义
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL, bytes.NewReader(data)) // 同样绑定 ctx 以支持取消
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json") // 固定内容类型
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp %s notify %s: %w", h.name, method, err) // 网络层失败：错误携带服务器与方法上下文
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body) // 即使不解析也要读完响应体，才能安全复用底层连接
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { // 通知没有协议层应答可查，HTTP 状态即成败依据
		return fmt.Errorf("mcp %s notify %s: http %d: %s", h.name, method, resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// ensureInit 执行 MCP initialize 握手（协议要求先于一切工具操作）：
// initialize 请求 → notifications/initialized 通知。sync.Once 保证只握手一次，
// 结果（含失败）被缓存 —— 失败的服务器不会被反复重试握手，是否致命由上层决定。
func (h *mcpHost) ensureInit(ctx context.Context) error {
	h.initOnce.Do(func() {
		// 按 MCP 规范申报协议版本、能力集与客户端身份。
		params := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "loom",
				"version": version,
			},
		}
		if _, err := h.call(ctx, "initialize", params); err != nil { // 第一步：initialize 请求；失败即记录并终止握手
			h.initErr = err
			return
		}
		if err := h.notify(ctx, "notifications/initialized", nil); err != nil { // 第二步：initialized 通知，宣告客户端就绪
			h.initErr = err
			return
		}
	})
	return h.initErr // 返回缓存的握手结果；nil 表示已就绪
}

// listTools 拉取该服务器的工具清单：先确保握手完成，再调 tools/list，
// 最后按白名单裁剪。返回的 ToolDef 将直接进入模型可见的工具集。
func (h *mcpHost) listTools(ctx context.Context) ([]contract.ToolDef, error) {
	if err := h.ensureInit(ctx); err != nil { // 握手失败则整台服务器不可用
		return nil, err
	}
	result, err := h.call(ctx, "tools/list", nil) // 拉取清单（无参方法）
	if err != nil {
		return nil, err
	}
	// 只解码 tools 字段；contract.ToolDef 与 MCP 的工具描述结构逐字段兼容。
	var resp struct {
		Tools []contract.ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	if h.filter == nil { // 未配置白名单：全量暴露
		return resp.Tools, nil
	}
	var filtered []contract.ToolDef
	for _, t := range resp.Tools { // 白名单裁剪：仅保留配置点名的工具
		if h.filter[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

// dispatch 把一次工具调用转发到该服务器并回填结果。约定：所有失败都折叠为
// IsError=true 的 ToolResult（而非 Go 错误），错误文本作为工具结果回给模型，
// 让模型有机会自行纠错或换路，而不是让整轮 agent 崩溃。
func (h *mcpHost) dispatch(ctx context.Context, call contract.ToolCall) *contract.ToolResult {
	if err := h.ensureInit(ctx); err != nil { // 握手未成的服务器：以错误结果回填
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	if h.filter != nil && !h.filter[call.Name] { // 白名单兜底校验：即使模型幻觉出未授权的工具名也会被拦下
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("tool %q is not available for this agent", call.Name), IsError: true}
	}
	// 转发 tools/call；入参 Args 本身已是 JSON 文本，用 RawMessage 原样嵌入避免二次转义。
	result, err := h.call(ctx, "tools/call", map[string]any{"name": call.Name, "arguments": json.RawMessage(call.Args)})
	if err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}
	}
	// MCP 的调用结果是内容块数组 + isError 标志；v1 只消费 text 类型的块。
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil { // 结果不符合标准结构：把原始 JSON 原样回给模型，宽容对待非常规服务器
		return &contract.ToolResult{CallID: call.ID, Content: string(result)}
	}
	var content string
	for _, c := range resp.Content { // 拼接全部 text 块作为工具输出；非文本块（图片等）v1 忽略
		if c.Type == "text" {
			content += c.Text
		}
	}
	return &contract.ToolResult{CallID: call.ID, Content: content, IsError: resp.IsError} // 透传服务器侧的 isError 语义
}

// mcpDispatcher aggregates one or more MCP servers behind contract.ToolDispatcher,
// routing each tool call to the server that advertised it (first server wins on
// a name collision).
// mcpDispatcher 把多台 MCP 服务器聚合为引擎眼中的单一工具分发器：
// 惰性一次性拉齐所有清单并建立"工具名 → 服务器"的路由表；重名工具先到先得。
type mcpDispatcher struct {
	servers []*mcpHost // 配置声明的全部服务器客户端

	mu     sync.Mutex          // 保护以下惰性初始化字段（工具循环可能并发调用）
	listed bool                // 清单是否已成功聚合；成功后不再重复拉取
	tools  []contract.ToolDef  // 聚合去重后的全量工具清单
	route  map[string]*mcpHost // 工具名 → 宣告该工具的服务器
}

// ensureListed 惰性聚合全部服务器的工具清单，是"弹性加载 + 必需服务器大声失败"
// 策略的核心：可选服务器失败仅记日志跳过；必需（required）服务器失败、或全军覆没，
// 才让整体失败。只有成功才置位 listed —— 失败不缓存，下次调用会重试，具备自愈能力。
func (d *mcpDispatcher) ensureListed(ctx context.Context) error {
	d.mu.Lock() // 全程持锁：既防并发重复拉取，也保护路由表的写入
	defer d.mu.Unlock()
	if d.listed { // 已成功聚合过：直接命中缓存
		return nil
	}
	route := make(map[string]*mcpHost) // 本轮新建路由表，成功后才整体发布到字段上
	var all []contract.ToolDef
	var loadedServers int        // 成功加载的服务器数
	var failedServers int        // 失败的服务器数
	var failures []error         // 全部失败明细（用于"全军覆没"时合并上报）
	var requiredFailures []error // 仅必需服务器的失败（任意一个即致命）
	for _, s := range d.servers {
		ts, err := s.listTools(ctx)
		if err != nil {
			failedServers++
			slog.Warn("loom/mcp: server skipped", "server", s.name, "error", err) // 弹性策略：单台失败先记警告，不立刻中止
			failure := fmt.Errorf("server %q failed: %w", s.name, err)
			failures = append(failures, failure)
			if s.required { // 必需服务器的失败单独归档，稍后触发整体失败
				requiredFailures = append(requiredFailures, fmt.Errorf("required %w", failure))
			}
			continue // 跳过失败服务器，继续加载其余
		}
		loadedServers++
		slog.Info("loom/mcp: server tools loaded", "server", s.name, "tools", len(ts))
		for _, t := range ts { // 合并进全局清单并登记路由；重名工具遵循"先到先得"
			if _, dup := route[t.Name]; dup {
				continue
			}
			route[t.Name] = s
			all = append(all, t)
		}
	}
	configuredServers := len(d.servers)
	// 汇总日志：一眼看清 配置数/成功数/失败数/工具总量。
	slog.Info("loom/mcp: loaded tools",
		"tools", len(all),
		"servers_loaded", loadedServers,
		"servers_configured", configuredServers,
		"servers_failed", failedServers)
	if configuredServers > 0 && len(all) == 0 { // 配了服务器却零工具：强异常信号，升级为 Error 级日志
		slog.Error("loom/mcp: configured servers loaded zero tools",
			"servers_configured", configuredServers,
			"servers_loaded", loadedServers,
			"servers_failed", failedServers)
	}
	if len(requiredFailures) > 0 { // fail loudly：任一必需服务器失败即让预检/首次调用整体失败
		return errors.Join(requiredFailures...)
	}
	if configuredServers > 0 && loadedServers == 0 { // 全部服务器都挂了：即便都非必需也视为致命
		return fmt.Errorf("all configured MCP servers failed: %w", errors.Join(failures...))
	}
	d.route, d.tools, d.listed = route, all, true // 成功：一次性发布路由表、清单与完成标记
	return nil
}

// Preflight 供引擎启动前预检：提前完成握手 + 清单聚合，把必需服务器的故障
// 暴露在任何模型调用发生之前（run.go 以接口断言探测到此方法后调用）。
func (d *mcpDispatcher) Preflight(ctx context.Context) error {
	return d.ensureListed(ctx)
}

// ListTools 返回聚合后的工具清单，供拼进 LLM 请求的工具声明。
func (d *mcpDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	if err := d.ensureListed(ctx); err != nil { // 首次调用触发惰性加载；失败可在后续调用中重试
		return nil, err
	}
	return d.tools, nil
}

// Dispatch 按路由表把工具调用转交给宣告过该工具的服务器。
func (d *mcpDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if err := d.ensureListed(ctx); err != nil { // 路由表未就绪时先补齐加载
		return nil, err
	}
	d.mu.Lock() // 短临界区查表：与 ensureListed 的表写入互斥
	s := d.route[call.Name]
	d.mu.Unlock()
	if s == nil { // 无人宣告过该工具（模型幻觉/清单漂移）：以错误结果回填而非崩溃
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}, nil
	}
	return s.dispatch(ctx, call), nil // 交给目标服务器执行；其内部已把失败折叠为 IsError 结果
}

// 编译期断言：mcpDispatcher 必须完整实现 ToolDispatcher 契约。
var _ contract.ToolDispatcher = (*mcpDispatcher)(nil)
