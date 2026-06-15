package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRunToolErrorClosesClient(t *testing.T) {
	serverBin := buildFakeMCPServer(t)
	marker := filepath.Join(t.TempDir(), "stdin-closed.txt")
	t.Setenv("MCP_LAUNCHER_FAKE_EXIT_MARKER", marker)

	exitCode := runTool(serverBin, t.TempDir(), "full", "failing", "{}", 2, 0, "", nil)
	if exitCode != 1 {
		t.Fatalf("runTool exit code = %d, want 1", exitCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake MCP server did not observe stdin close; marker %s was not written", marker)
}

func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fake_mcp_server.go")
	binary := filepath.Join(dir, "fake-mcp-server")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(source, []byte(fakeMCPServerSource), 0o644); err != nil {
		t.Fatalf("write fake server source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", binary, source)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fake MCP server: %v\n%s", err, string(out))
	}
	return binary
}

const fakeMCPServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	marker := os.Getenv("MCP_LAUNCHER_FAKE_EXIT_MARKER")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		id, hasID := request["id"]
		method, _ := request["method"].(string)
		if !hasID {
			continue
		}
		switch method {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo": map[string]any{"name": "fake", "version": "test"},
					"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
				},
			})
		case "tools/list":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{"tools": []any{}},
			})
		case "tools/call":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "fake tool failure"}},
					"isError": true,
				},
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id": id,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
	if marker != "" {
		_ = os.WriteFile(marker, []byte("closed"), 0o644)
	}
}
`
