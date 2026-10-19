package localtools

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncodePowerShellIsUTF16Base64(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(encodePowerShell("echo '你好' \"x\""))
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("raw = %v, %v", raw, err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(units)); got != "echo '你好' \"x\"" {
		t.Fatalf("decoded = %q", got)
	}
}

func TestShellForNamesByExecutable(t *testing.T) {
	for path, want := range map[string]string{
		`C:\Program Files\PowerShell\7\pwsh.exe`:                    "PowerShell",
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`: "PowerShell",
		`C:\Windows\System32\cmd.exe`:                               "cmd",
		"/bin/sh":                                                   "sh",
		"/usr/local/bin/bash":                                       "bash",
	} {
		if got := shellFor(path).Name; got != want {
			t.Errorf("shellFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPowerShellCommandUsesEncodedArgumentOrScriptFile(t *testing.T) {
	cmd, cleanup, err := powerShellCommand("powershell.exe", "Write-Output 1")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if args := strings.Join(cmd.Args, " "); !strings.Contains(args, "-NonInteractive") || !strings.Contains(args, "-EncodedCommand") {
		t.Fatalf("args = %s", args)
	}
	long := strings.Repeat("$null = 1\n", 2000) + "Write-Output done"
	cmd, cleanup, err = powerShellCommand("powershell.exe", long)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if args := strings.Join(cmd.Args, " "); !strings.Contains(args, "-File") || strings.Contains(args, "-EncodedCommand") {
		t.Fatalf("a long script must run from a file: %s", args[:80])
	}
}
