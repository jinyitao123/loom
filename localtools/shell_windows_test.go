//go:build windows

package localtools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func powerShellTools(t *testing.T) (*Tools, string) {
	t.Helper()
	shell := detectShell("powershell")
	if shell.Name != "PowerShell" {
		t.Skip("PowerShell is not installed")
	}
	tools, dir := newTools(t, All...)
	tools.shell = shell
	return tools, dir
}

func TestPowerShellRunsCommands(t *testing.T) {
	tools, dir := powerShellTools(t)

	hello := call(t, tools, "bash", map[string]any{"command": "Write-Output '你好，世界'"})
	if hello.IsError || strings.TrimSpace(hello.Content) != "你好，世界" {
		t.Fatalf("unicode output = %+v", hello)
	}
	quoted := call(t, tools, "bash", map[string]any{"command": `$a = "it's"; Write-Output "$a ""quoted"" & done"`})
	if strings.TrimSpace(quoted.Content) != `it's "quoted" & done` {
		t.Fatalf("quoting = %q", quoted.Content)
	}
	where := call(t, tools, "bash", map[string]any{"command": "(Get-Location).Path"})
	want, _ := filepath.EvalSymlinks(dir)
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(where.Content)); !strings.EqualFold(got, want) {
		t.Fatalf("working directory = %q, want %q", where.Content, want)
	}
	exit := call(t, tools, "bash", map[string]any{"command": "exit 3"})
	if !exit.IsError || !strings.Contains(exit.Content, "exited with code 3") {
		t.Fatalf("exit 3 = %+v", exit)
	}
	failed := call(t, tools, "bash", map[string]any{"command": `Get-Item 'C:\loom-no-such-path'`})
	if !failed.IsError || !strings.Contains(failed.Content, "exited with code 1") {
		t.Fatalf("failing cmdlet = %+v", failed)
	}
	slow := call(t, tools, "bash", map[string]any{"command": "Start-Sleep -Seconds 10", "timeout": 1.5})
	if !slow.IsError || !strings.Contains(slow.Content, "timed out") {
		t.Fatalf("timeout = %+v", slow)
	}
}

func TestPowerShellRunsLongScriptsFromAFile(t *testing.T) {
	tools, _ := powerShellTools(t)
	long := strings.Repeat("$null = 1\n", 2000) + "Write-Output '长脚本完成'"
	result := call(t, tools, "bash", map[string]any{"command": long})
	if result.IsError || strings.TrimSpace(result.Content) != "长脚本完成" {
		t.Fatalf("long script = %+v", result)
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "loom-command-*.ps1"))
	if len(matches) > 0 {
		t.Fatalf("temporary scripts left behind: %v", matches)
	}
}

func TestDetectShellOnWindows(t *testing.T) {
	if !notWSL(`C:\Program Files\Git\bin\bash.exe`) || notWSL(`C:\Windows\System32\bash.exe`) || notWSL(`C:\Users\a\AppData\Local\Microsoft\WindowsApps\bash.exe`) {
		t.Fatal("WSL bash must be rejected")
	}
	if got := detectShell("cmd"); got.Name != "cmd" {
		t.Fatalf("cmd override = %+v", got)
	}
	if got := detectShell(""); got.Name == "bash" && !notWSL(got.Path) {
		t.Fatalf("detected WSL bash: %s", got.Path)
	}
}
