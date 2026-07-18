// Command loom is a thin CLI wrapper around the Loom engine that speaks the
// weave daemon's spawn-harness protocol: it reads a prompt JSON on stdin, runs
// an agent turn, and emits a stdout-json (NDJSON) event stream the daemon's
// LoomBackend parses (see weave src/cli/daemon/agent/loom.ts and
// plans/loom-cli-design.md).
//
// Usage:
//
//	loom run --format stream-json --cwd <dir> --bypass-permissions \
//	         [--model deepseek-chat] [--resume <sessionId>] < prompt.json
//	loom version
//
// 中文补充：loom 是 Loom 引擎的薄 CLI 封装，实现 weave 守护进程的
// spawn-harness 协议——从 stdin 读入 prompt JSON，运行一轮 agent，
// 向 stdout 输出 NDJSON 事件流，由守护进程侧的 LoomBackend 逐行解析。
package main

import (
	"fmt" // 打印版本号与 stderr 上的用法/错误信息
	"os"  // 读取命令行参数、访问标准流、设置进程退出码
)

// version is injected at release time via -ldflags "-X main.version=...".
// Defaults to "dev" for local builds.
// 中文补充：version 在发布构建时由链接器注入真实版本号；
// 本地构建保持默认值 "dev"，用于区分开发版与正式发布版。
var version = "dev"

// main 只做子命令分发：解析第一个位置参数并转交给对应实现；
// 真正的 agent 运行逻辑在 runCmd（run.go）中。
func main() {
	// 未提供子命令：向 stderr 打印用法说明，并以退出码 2（"用法错误"的惯例值）退出。
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loom <command>\n\ncommands:\n  run        run one agent turn (reads prompt JSON from stdin)\n  version    print version")
		os.Exit(2)
	}
	// 以 os.Args[1] 作为子命令名分发；其余参数原样透传给子命令自行解析。
	switch os.Args[1] {
	case "run":
		// run 子命令：跑一轮 agent（stdin 读 prompt JSON）；runCmd 的返回值
		// 直接作为进程退出码，宿主（weave 守护进程）据此判定本轮运行成败。
		os.Exit(runCmd(os.Args[2:]))
	case "version", "--version", "-v":
		// version 子命令：打印版本号；同时兼容 --version / -v 两种常见写法。
		fmt.Printf("loom %s\n", version)
	default:
		// 未知子命令：报错到 stderr 并以退出码 2 退出，与"用法错误"语义保持一致。
		fmt.Fprintf(os.Stderr, "loom: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
