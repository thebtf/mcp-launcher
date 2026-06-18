// mcp-launcher emulates how Claude Code and Codex spawn MCP servers as stdio
// subprocesses. Use it to test graceful restart, hot-swap handoff, session
// lifecycle, and MCP protocol compliance without a live AI client.
//
// Modes:
//
//	hold   — spawn server, hold session open for external testing (default)
//	call   — call any JSON-RPC method after MCP initialize
//	tool   — call any MCP tool by name with JSON arguments
//	resource — read any MCP resource by URI
//	install — install a local binary through the server's upgrade tool, then reconnect and verify health
//	test   — single-phase: daemon + owner + graceful-restart + verify
//	phase2 — two-phase: bootstrap + restart on successor (deadlock reproducer)
//	persist — verify daemon survives stdio disconnect and reconnect
//	kill-reconnect — hard-kill daemon and measure new-session recovery
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 minimal types
// ---------------------------------------------------------------------------

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (m *jsonRPCMessage) isResponse() bool     { return m.ID != nil && m.Method == "" }
func (m *jsonRPCMessage) isNotification() bool { return m.ID == nil && m.Method != "" }

// ---------------------------------------------------------------------------
// MCP client — spawns server, manages JSON-RPC over stdio
// ---------------------------------------------------------------------------

type mcpClient struct {
	cmd     *exec.Cmd
	stdin   *os.File
	scanner *bufio.Scanner

	mu                 sync.Mutex
	nextID             int
	pending            map[int]chan json.RawMessage
	notifCh            chan jsonRPCMessage
	closeCh            chan struct{}
	closeOnce          sync.Once
	invalidStdoutLines int
}

type sessionInfo struct {
	ToolCount     int
	ServerName    string
	ServerVersion string
}

var sessionRequestTimeout = 30 * time.Second
var runCommandCombinedOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

const (
	defaultInstallReconnectDelaySec  = 2
	postExitInstallReconnectDelaySec = 15
	installValidationReplacement     = "replacement"
	installValidationActivePointer   = "active-pointer"
	activeEngineFileEnv              = "MCPMUX_ACTIVE_ENGINE_FILE"
)

type fileFingerprint struct {
	Size    int64
	ModTime time.Time
	SHA256  string
}

func newMCPClient(binary, cwd, envMode string, extraArgs []string) (*mcpClient, error) {
	return newMCPClientWithEnv(binary, cwd, envForMode(envMode), extraArgs)
}

func newMCPClientWithEnv(binary, cwd string, env []string, extraArgs []string) (*mcpClient, error) {
	cmd := exec.Command(binary, extraArgs...)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr
	cmd.Env = env

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	c := &mcpClient{
		cmd:     cmd,
		stdin:   stdinPipe.(*os.File),
		scanner: bufio.NewScanner(stdoutPipe),
		nextID:  1,
		pending: make(map[int]chan json.RawMessage),
		notifCh: make(chan jsonRPCMessage, 100),
		closeCh: make(chan struct{}),
	}
	c.scanner.Buffer(make([]byte, 1<<20), 1<<20)

	go c.readLoop()
	return c, nil
}

func envForMode(envMode string) []string {
	switch envMode {
	case "full":
		// CC behavior: pass full parent env
		return os.Environ()
	case "clean":
		// Codex behavior: platform allow-list only
		return cleanEnv()
	default:
		return os.Environ()
	}
}

func (c *mcpClient) readLoop() {
	defer c.closeOnce.Do(func() { close(c.closeCh) })
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg jsonRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			c.mu.Lock()
			c.invalidStdoutLines++
			c.mu.Unlock()
			continue
		}
		if msg.isResponse() {
			var id int
			if err := json.Unmarshal(msg.ID, &id); err == nil {
				c.mu.Lock()
				ch, ok := c.pending[id]
				if ok {
					delete(c.pending, id)
				}
				c.mu.Unlock()
				if ok {
					ch <- json.RawMessage(line)
				}
			}
		} else if msg.isNotification() {
			select {
			case c.notifCh <- msg:
			default:
			}
		}
	}
}

func (c *mcpClient) invalidStdoutCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalidStdoutLines
}

func (c *mcpClient) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	data, _ := json.Marshal(msg)
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for %s (id=%d) after %v", method, id, timeout)
	case <-c.closeCh:
		return nil, fmt.Errorf("connection closed while waiting for %s (id=%d)", method, id)
	}
}

func (c *mcpClient) notify(method string, params any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	data, _ := json.Marshal(msg)
	data = append(data, '\n')
	c.stdin.Write(data)
}

func (c *mcpClient) close() {
	c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
}

func (c *mcpClient) pid() int { return c.cmd.Process.Pid }

func callTool(client *mcpClient, name string, args map[string]any, timeout time.Duration) (json.RawMessage, any, error) {
	resp, err := client.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, timeout)
	if err != nil {
		return nil, nil, err
	}

	payload, err := extractToolPayload(resp)
	if err != nil {
		return resp, nil, err
	}
	return resp, payload, nil
}

func readResource(client *mcpClient, uri string, timeout time.Duration) (json.RawMessage, any, error) {
	resp, err := client.call("resources/read", map[string]any{"uri": uri}, timeout)
	if err != nil {
		return nil, nil, err
	}

	payload, err := extractResourcePayload(resp)
	if err != nil {
		return resp, nil, err
	}
	return resp, payload, nil
}

func extractToolPayload(resp json.RawMessage) (any, error) {
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("decode tools/call response: %w", err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, fmt.Errorf("json-rpc error: %s", string(envelope.Error))
	}
	if envelope.Result.IsError {
		return nil, fmt.Errorf("tool returned error: %s", firstToolText(envelope.Result.Content))
	}
	if len(envelope.Result.Content) == 0 {
		return map[string]any{"content": []any{}}, nil
	}
	text := envelope.Result.Content[0].Text
	var payload any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload, nil
	}
	return map[string]any{"text": text}, nil
}

func firstToolText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	if len(content) == 0 {
		return "<empty tool error>"
	}
	return content[0].Text
}

func extractResourcePayload(resp json.RawMessage) (any, error) {
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("decode resources/read response: %w", err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, fmt.Errorf("json-rpc error: %s", string(envelope.Error))
	}
	if len(envelope.Result.Contents) == 0 {
		return map[string]any{"contents": []any{}}, nil
	}
	text := envelope.Result.Contents[0].Text
	var payload any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload, nil
	}
	return map[string]any{"text": text}, nil
}

func parseJSONObjectFlag(name, value string) map[string]any {
	if strings.TrimSpace(value) == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "error: -%s must be a JSON object: %v\n", name, err)
		os.Exit(1)
	}
	return parsed
}

func parseJSONValueFlag(name, value string) any {
	if strings.TrimSpace(value) == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "error: -%s must be valid JSON: %v\n", name, err)
		os.Exit(1)
	}
	return parsed
}

func printJSON(label string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Printf("%s: %v\n", label, value)
		return
	}
	fmt.Printf("%s:\n%s\n", label, string(data))
}

func printRawJSON(label string, raw json.RawMessage) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		fmt.Printf("%s: %s\n", label, string(raw))
		return
	}
	printJSON(label, value)
}

// ---------------------------------------------------------------------------
// Control socket client — sends commands to daemon control socket
// ---------------------------------------------------------------------------

func controlSend(socketPath, cmd string, timeout time.Duration) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socketPath, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := map[string]any{"cmd": cmd, "drain_timeout_ms": 10000}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			return nil, fmt.Errorf("read: %w", scanner.Err())
		}
		return nil, fmt.Errorf("read: unexpected EOF")
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}

func getDaemonPID(ctlSocket string) (int, error) {
	resp, err := controlSend(ctlSocket, "status", 5*time.Second)
	if err != nil {
		return -1, err
	}

	toInt := func(v any) (int, bool) {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		}
		return -1, false
	}

	if pid, ok := toInt(resp["pid"]); ok {
		return pid, nil
	}
	if pid, ok := toInt(resp["daemon_pid"]); ok {
		return pid, nil
	}
	if daemon, ok := resp["daemon"].(map[string]any); ok {
		if pid, ok := toInt(daemon["pid"]); ok {
			return pid, nil
		}
	}
	// muxcore v0.22.1+ wraps the status payload under "data" and exposes
	// pid: os.Getpid() at the top level of that sub-map.
	if data, ok := resp["data"].(map[string]any); ok {
		if pid, ok := toInt(data["pid"]); ok {
			return pid, nil
		}
	}

	return -1, fmt.Errorf("no pid field in status response: %v", resp)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	return runtime.GOOS != "windows"
}

func cleanupBinaryProcesses(binary string) error {
	name := filepath.Base(binary)
	if strings.TrimSpace(name) == "" || name == "." || name == string(filepath.Separator) {
		return fmt.Errorf("cannot derive process image name from %q", binary)
	}

	switch runtime.GOOS {
	case "windows":
		out, err := runCommandCombinedOutput("taskkill", "/F", "/IM", name)
		if err != nil {
			output := strings.TrimSpace(string(out))
			if strings.Contains(strings.ToLower(output), "not found") {
				return nil
			}
			if fallbackErr := cleanupWindowsProcessesByImageWithPIDFallback(name); fallbackErr == nil {
				return nil
			} else {
				return fmt.Errorf("taskkill %s: %w: %s; PID fallback failed: %v", name, err, output, fallbackErr)
			}
		}
		return nil
	default:
		return fmt.Errorf("cleanup-binary-processes is not implemented on %s", runtime.GOOS)
	}
}

func cleanupWindowsProcessesByImageWithPIDFallback(name string) error {
	pids, err := listWindowsProcessIDsByImage(name)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}

	var failures []string
	for _, pid := range pids {
		if err := stopWindowsProcessByID(pid); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func listWindowsProcessIDsByImage(name string) ([]int, error) {
	escapedName := strings.ReplaceAll(name, "'", "''")
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'; Get-CimInstance Win32_Process -Filter "Name = '%s'" | ForEach-Object { [int]$_.ProcessId }`, escapedName)
	out, err := runCommandCombinedOutput("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return nil, formatCommandError("enumerate Windows processes", err, out)
	}

	var pids []int
	for _, line := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return nil, fmt.Errorf("parse Windows process id %q: %w", line, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func stopWindowsProcessByID(pid int) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'; Stop-Process -Id %d -Force -ErrorAction Stop`, pid)
	out, err := runCommandCombinedOutput("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return formatCommandError(fmt.Sprintf("Stop-Process -Id %d", pid), err, out)
	}
	return nil
}

func formatCommandError(action string, err error, out []byte) error {
	output := strings.TrimSpace(string(out))
	if output == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, output)
}

// ---------------------------------------------------------------------------
// Clean env (Codex-style platform allow-list)
// ---------------------------------------------------------------------------

func cleanEnv() []string {
	var keys []string
	if runtime.GOOS == "windows" {
		keys = []string{
			"PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "SYSTEMDRIVE",
			"USERNAME", "USERDOMAIN", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
			"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "PROGRAMDATA",
			"LOCALAPPDATA", "APPDATA", "TEMP", "TMP",
		}
	} else {
		keys = []string{
			"HOME", "LOGNAME", "PATH", "SHELL", "USER",
			"LANG", "LC_ALL", "TERM", "TMPDIR", "TZ",
		}
	}
	keys = append(keys,
		"AIMUX_STDIN_EOF_POLICY",
		"AIMUX_ENGINE_NAME",
		"AIMUX_SESSION_STORE",
		"AIMUX_WARMUP",
		"AIMUX_CONFIG_DIR",
		"AIMUX_NO_ENGINE",
		"AIMUX_POST_EXIT_HELPER_DIR",
		"AIMUX_UPGRADE_SOURCE_DIR",
		"AIMUX_ALLOW_UPGRADE_SOURCE_OUTSIDE_BIN_DIR",
		activeEngineFileEnv,
	)
	var env []string
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// ---------------------------------------------------------------------------
// Session bootstrap — shared by all modes
// ---------------------------------------------------------------------------

func tryInitSession(binary, cwd, envMode string, extraArgs []string, expectTools int, expectVersion string) (*mcpClient, sessionInfo, error) {
	info := sessionInfo{}
	fmt.Printf("  spawn %s %v (cwd=%s, env=%s)\n", binary, extraArgs, cwd, envMode)
	client, err := newMCPClient(binary, cwd, envMode, extraArgs)
	if err != nil {
		return nil, info, fmt.Errorf("spawn: %w", err)
	}
	fmt.Printf("  pid=%d\n", client.pid())

	resp, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "mcp-launcher", "version": "1.0.0"},
		"capabilities":    map[string]any{"roots": map[string]any{}},
	}, sessionRequestTimeout)
	if err != nil {
		client.close()
		return nil, info, fmt.Errorf("initialize: %w", err)
	}
	var initEnvelope struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &initEnvelope); err == nil {
		info.ServerName = initEnvelope.Result.ServerInfo.Name
		info.ServerVersion = initEnvelope.Result.ServerInfo.Version
	}
	fmt.Printf("  initialize: %s\n", truncate(string(resp), 120))
	if expectVersion != "" && info.ServerVersion != expectVersion {
		client.close()
		return nil, info, fmt.Errorf("server version: got %q, want %q", info.ServerVersion, expectVersion)
	}

	client.notify("notifications/initialized", map[string]any{})

	resp, err = client.call("tools/list", map[string]any{}, sessionRequestTimeout)
	if err != nil {
		client.close()
		return nil, info, fmt.Errorf("tools/list: %w", err)
	}
	var tr struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(resp, &tr)
	info.ToolCount = len(tr.Result.Tools)
	fmt.Printf("  tools: %d\n", info.ToolCount)
	if expectTools > 0 && info.ToolCount != expectTools {
		client.close()
		return nil, info, fmt.Errorf("tools count: got %d, want %d", info.ToolCount, expectTools)
	}

	return client, info, nil
}

func initSession(binary, cwd, envMode string, extraArgs []string, expectTools int, expectVersion string) (*mcpClient, sessionInfo) {
	client, info, err := tryInitSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL %v\n", err)
		os.Exit(1)
	}
	return client, info
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

func runHold(binary, cwd, envMode string, holdSec, expectTools int, expectVersion string, extraArgs []string) {
	fmt.Println("[hold] Starting MCP session...")
	client, _ := initSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
	defer client.close()

	fmt.Printf("[hold] Session live (pid=%d). Holding for %ds.\n", client.pid(), holdSec)
	fmt.Println("[hold] Use your control tool to test the daemon while this session is active.")

	select {
	case <-time.After(time.Duration(holdSec) * time.Second):
		fmt.Println("[hold] Timeout reached, closing.")
	case <-client.closeCh:
		fmt.Println("[hold] Server closed connection.")
	}
}

func runTest(binary, cwd, envMode, ctlSocket, daemonFlag string, extraArgs []string) {
	fmt.Println("[test] Start daemon + session, trigger graceful-restart, verify")

	daemon := exec.Command(binary, daemonFlag)
	daemon.Dir = cwd
	daemon.Env = os.Environ()
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL start daemon: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  daemon pid=%d\n", daemon.Process.Pid)
	time.Sleep(3 * time.Second)

	client, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")
	time.Sleep(3 * time.Second)

	status, err := controlSend(ctlSocket, "status", 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL status: %v\n", err)
		client.close()
		os.Exit(1)
	}
	fmt.Printf("  owner_count=%v\n", status["owner_count"])

	fmt.Println("  sending graceful-restart...")
	start := time.Now()
	resp, err := controlSend(ctlSocket, "graceful-restart", 60*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("  FAIL graceful-restart: %v (after %v)\n", err, elapsed)
	} else {
		fmt.Printf("  OK: ok=%v message=%v (%v)\n", resp["ok"], resp["message"], elapsed)
	}

	time.Sleep(5 * time.Second)

	status, err = controlSend(ctlSocket, "status", 5*time.Second)
	if err != nil {
		fmt.Printf("  new daemon status: FAIL %v\n", err)
	} else {
		fmt.Printf("  new daemon: owner_count=%v handoff=%v\n", status["owner_count"], status["handoff"])
	}

	client.close()
	fmt.Println("[test] Done.")
}

func runPhase2(binary, cwd, envMode, ctlSocket, daemonFlag string, extraArgs []string) {
	fmt.Println("[phase2] Full test: Phase 1 bootstrap + Phase 2 on successor")

	daemon := exec.Command(binary, daemonFlag)
	daemon.Dir = cwd
	daemon.Env = os.Environ()
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL start daemon: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  daemon pid=%d\n", daemon.Process.Pid)
	time.Sleep(3 * time.Second)

	client, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")
	time.Sleep(3 * time.Second)

	fmt.Println("\n--- Phase 1: graceful-restart ---")
	start := time.Now()
	resp, err := controlSend(ctlSocket, "graceful-restart", 60*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("  Phase 1 FAIL: %v (%v)\n", err, elapsed)
	} else {
		fmt.Printf("  Phase 1 OK: %v (%v)\n", resp["message"], elapsed)
	}

	time.Sleep(10 * time.Second)

	fmt.Println("\n--- Phase 2: graceful-restart on successor ---")
	start = time.Now()
	resp, err = controlSend(ctlSocket, "graceful-restart", 60*time.Second)
	elapsed = time.Since(start)
	if err != nil {
		fmt.Printf("  Phase 2 FAIL: %v (%v)\n", err, elapsed)
	} else {
		fmt.Printf("  Phase 2 OK: %v (%v)\n", resp["message"], elapsed)
	}

	time.Sleep(3 * time.Second)

	status, err := controlSend(ctlSocket, "status", 5*time.Second)
	if err != nil {
		fmt.Printf("  final status: FAIL %v\n", err)
	} else {
		fmt.Printf("  final: owner_count=%v handoff=%v\n", status["owner_count"], status["handoff"])
	}

	client.close()
	fmt.Println("[phase2] Done.")
}

func runPersist(binary, cwd, envMode, ctlSocket, daemonFlag string, watchSec int, extraArgs []string) {
	fmt.Println("[persist] Start daemon + session, disconnect stdio, verify daemon persistence")

	daemon := exec.Command(binary, daemonFlag)
	daemon.Dir = cwd
	daemon.Env = os.Environ()
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL start daemon: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  daemon pid=%d\n", daemon.Process.Pid)
	time.Sleep(3 * time.Second)

	fmt.Println("  Session A: connect")
	clientA, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")

	daemonPIDA, err := getDaemonPID(ctlSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL daemon pid after Session A: %v\n", err)
		clientA.close()
		os.Exit(1)
	}
	fmt.Printf("  Session A daemon pid=%d\n", daemonPIDA)

	fmt.Println("  Session A: closing stdio")
	clientA.close()

	start := time.Now()
	fmt.Printf("  watching daemon for %ds\n", watchSec)

	checkAlive := func(elapsed time.Duration) bool {
		alive := pidAlive(daemonPIDA)
		fmt.Printf("  [t+%ds] daemon pid=%d alive=%v\n", int(elapsed.Seconds()), daemonPIDA, alive)
		if !alive {
			fmt.Printf("  FAIL: daemon pid %d died during watch window\n", daemonPIDA)
			return false
		}
		return true
	}

	if !checkAlive(0) {
		os.Exit(1)
	}

	watchDeadline := start.Add(time.Duration(watchSec) * time.Second)
	for {
		remaining := time.Until(watchDeadline)
		if remaining <= 0 {
			break
		}

		wait := 5 * time.Second
		if remaining < wait {
			wait = remaining
		}

		time.Sleep(wait)
		if !checkAlive(time.Since(start)) {
			os.Exit(1)
		}
	}

	fmt.Println("  Session B: reconnect")
	clientB, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")
	defer clientB.close()

	daemonPIDB, err := getDaemonPID(ctlSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL daemon pid after Session B: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Session B daemon pid=%d\n", daemonPIDB)

	if daemonPIDA == daemonPIDB {
		fmt.Printf("  PASS: daemon pid %d stayed alive for %ds and reconnect reused same pid\n", daemonPIDA, watchSec)
		return
	}

	fmt.Printf("  FAIL: daemon pid changed across reconnect (A=%d, B=%d)\n", daemonPIDA, daemonPIDB)
	os.Exit(1)
}

func killProcess(pid int) error {
	if runtime.GOOS == "windows" {
		return exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGKILL)
}

func waitDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !pidAlive(pid)
}

func runKillReconnect(binary, cwd, envMode, ctlSocket, _ string, extraArgs []string) {
	fmt.Println("[kill-reconnect] Reproduce mcp-mux #104: hard-kill daemon, measure new-session recovery time")
	fmt.Println("  (daemon is spawned ad-hoc by shim's ensureDaemon — no explicit daemon launch)")

	fmt.Println("  Session A: connect")
	clientA, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")
	time.Sleep(2 * time.Second) // let daemon settle

	daemonPIDA, err := getDaemonPID(ctlSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL daemon pid after Session A: %v\n", err)
		clientA.close()
		os.Exit(1)
	}
	fmt.Printf("  daemon pid (alive)=%d\n", daemonPIDA)

	fmt.Printf("  hard-killing daemon pid=%d\n", daemonPIDA)
	if err := killProcess(daemonPIDA); err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL kill: %v\n", err)
		clientA.close()
		os.Exit(1)
	}
	if !waitDead(daemonPIDA, 5*time.Second) {
		fmt.Fprintf(os.Stderr, "  FAIL daemon pid %d still alive after 5s\n", daemonPIDA)
		clientA.close()
		os.Exit(1)
	}
	fmt.Printf("  daemon dead\n")

	fmt.Println("  Session A: closing stdio (simulating CC stdio drop)")
	clientA.close()

	fmt.Println("\n=== Phase 1: spawn Session B (measures total recovery: daemon respawn + handshake + tools/list) ===")
	start := time.Now()
	clientB, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")
	elapsed := time.Since(start)
	defer clientB.close()

	daemonPIDB, err := getDaemonPID(ctlSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL daemon pid after Session B: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n  total_recovery=%v daemon_pid_A=%d daemon_pid_B=%d\n", elapsed, daemonPIDA, daemonPIDB)

	if daemonPIDA == daemonPIDB {
		fmt.Printf("  WARN: daemon pid did not change — kill may have failed silently\n")
	}

	if elapsed < 30*time.Second {
		fmt.Printf("  PASS — recovery within CC stdio 30s timeout\n")
	} else {
		fmt.Printf("  FAIL — recovery exceeded 30s CC stdio timeout (took %v)\n", elapsed)
		os.Exit(2)
	}
}

func runRawCall(binary, cwd, envMode, method, paramsJSON string, timeoutSec, expectTools int, expectVersion string, extraArgs []string) int {
	if method == "" {
		fmt.Fprintln(os.Stderr, "error: -method is required for call mode")
		return 1
	}
	params := parseJSONValueFlag("params", paramsJSON)

	fmt.Printf("[call] %s\n", method)
	client, _ := initSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
	defer client.close()

	resp, err := client.call(method, params, time.Duration(timeoutSec)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL call %s: %v\n", method, err)
		return 1
	}
	printRawJSON("  response", resp)
	return 0
}

func runTool(binary, cwd, envMode, toolName, argsJSON string, timeoutSec, expectTools int, expectVersion string, extraArgs []string) int {
	if toolName == "" {
		fmt.Fprintln(os.Stderr, "error: -tool is required for tool mode")
		return 1
	}
	args := parseJSONObjectFlag("args", argsJSON)

	fmt.Printf("[tool] %s\n", toolName)
	client, _ := initSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
	defer client.close()

	resp, payload, err := callTool(client, toolName, args, time.Duration(timeoutSec)*time.Second)
	if err != nil {
		if resp != nil {
			printRawJSON("  response", resp)
		}
		fmt.Fprintf(os.Stderr, "  FAIL tool %s: %v\n", toolName, err)
		return 1
	}
	printRawJSON("  response", resp)
	printJSON("  payload", payload)
	return 0
}

func runResource(binary, cwd, envMode, uri string, timeoutSec, expectTools int, expectVersion string, extraArgs []string) int {
	if uri == "" {
		fmt.Fprintln(os.Stderr, "error: -uri is required for resource mode")
		return 1
	}

	fmt.Printf("[resource] %s\n", uri)
	client, _ := initSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
	defer client.close()

	resp, payload, err := readResource(client, uri, time.Duration(timeoutSec)*time.Second)
	if err != nil {
		if resp != nil {
			printRawJSON("  response", resp)
		}
		fmt.Fprintf(os.Stderr, "  FAIL resource %s: %v\n", uri, err)
		return 1
	}
	printRawJSON("  response", resp)
	printJSON("  payload", payload)
	return 0
}

func runInstall(binary, cwd, envMode, source, upgradeMode, installValidation, activeEngineFile string, force bool, timeoutSec, reconnectDelaySec int, reconnectDelayExplicit bool, cleanupBinary bool, expectTools int, expectVersion string, extraArgs []string) int {
	if source == "" {
		fmt.Fprintln(os.Stderr, "error: -source is required for install mode")
		return 1
	}
	installValidation, err := normalizeInstallValidation(installValidation)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	initialBinary, err := fingerprintFile(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fingerprint installed binary: %v\n", err)
		return 1
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve -source: %v\n", err)
		return 1
	}
	if _, err := os.Stat(absSource); err != nil {
		fmt.Fprintf(os.Stderr, "error: source binary is not readable: %v\n", err)
		return 1
	}

	initialActivePointer := ""
	if installValidation == installValidationActivePointer {
		activeEngineFileExplicit := activeEngineFile != ""
		if activeEngineFile == "" {
			activeEngineFile = os.Getenv(activeEngineFileEnv)
		}
		if activeEngineFile == "" {
			fmt.Fprintf(os.Stderr, "error: -active-engine-file or %s is required for -install-validation active-pointer\n", activeEngineFileEnv)
			return 1
		}
		activeEngineFile, err = filepath.Abs(activeEngineFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve -active-engine-file: %v\n", err)
			return 1
		}
		if activeEngineFileExplicit {
			if err := os.Setenv(activeEngineFileEnv, activeEngineFile); err != nil {
				fmt.Fprintf(os.Stderr, "error: set %s: %v\n", activeEngineFileEnv, err)
				return 1
			}
		}
		initialActivePointer, err = readInstallActivePointer(activeEngineFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read active engine file: %v\n", err)
			return 1
		}
		fmt.Printf("[install] Active-pointer validation using %s (initial %q)\n", activeEngineFile, initialActivePointer)
	}

	fmt.Println("[install] Starting installed daemon session")
	client, _ := initSession(binary, cwd, envMode, extraArgs, 0, "")

	args := map[string]any{
		"action": "apply",
		"source": absSource,
		"force":  force,
	}
	if upgradeMode != "" {
		args["mode"] = upgradeMode
	}

	fmt.Printf("[install] Calling upgrade(action=apply, source=%q, force=%v, mode=%q)\n", absSource, force, upgradeMode)
	resp, payload, err := callTool(client, "upgrade", args, time.Duration(timeoutSec)*time.Second)
	upgradeDisconnected := false
	if err != nil {
		if isExpectedUpgradeDisconnect(err) {
			upgradeDisconnected = true
			fmt.Printf("  upgrade connection closed during apply: %v\n", err)
			fmt.Println("  continuing to reconnect verification")
		} else {
			if resp != nil {
				printRawJSON("  upgrade response", resp)
			}
			fmt.Fprintf(os.Stderr, "  FAIL upgrade: %v\n", err)
			client.close()
			return 1
		}
	} else {
		printJSON("  upgrade payload", payload)
	}

	reconnectDelaySec = effectiveInstallReconnectDelaySec(reconnectDelaySec, reconnectDelayExplicit, payload)
	waitForReplacement := shouldWaitForInstallReplacement(installValidation, payload, upgradeDisconnected)
	waitForActivePointer := shouldWaitForInstallActivePointer(installValidation)

	fmt.Println("[install] Closing install session")
	client.close()

	if cleanupBinary && (waitForReplacement || waitForActivePointer || isPostExitInstallScheduled(payload)) {
		if err := cleanupBinaryProcesses(binary); err != nil {
			fmt.Fprintf(os.Stderr, "  WARN cleanup-binary-processes before reconnect: %v\n", err)
		}
	}

	if waitForReplacement {
		if err := waitForInstallBinaryReplacement(binary, initialBinary, timeoutSec, reconnectDelaySec); err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL install replacement: %v\n", err)
			return 1
		}
		reconnectDelaySec = 0
	}

	if waitForActivePointer {
		if _, err := waitForInstallActivePointerUpdate(activeEngineFile, initialActivePointer, timeoutSec, reconnectDelaySec); err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL active-pointer install: %v\n", err)
			return 1
		}
	}

	fmt.Println("[install] Reconnecting and verifying installed daemon")
	verifyClient, info, healthPayload, err := waitForInstallReconnect(binary, cwd, envMode, timeoutSec, reconnectDelaySec, expectTools, expectVersion, extraArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL reconnect verification: %v\n", err)
		return 1
	}
	defer verifyClient.close()
	if info.ServerVersion != "" {
		fmt.Printf("  verified server version: %s\n", info.ServerVersion)
	}

	printJSON("  sessions health", healthPayload)

	if _, resourcePayload, resourceErr := readResource(verifyClient, "aimux://health", sessionRequestTimeout); resourceErr == nil {
		printJSON("  aimux://health", resourcePayload)
	} else {
		fmt.Printf("  WARN aimux://health read failed: %v\n", resourceErr)
	}

	fmt.Println("[install] PASS")
	return 0
}

func waitForInstallReconnect(binary, cwd, envMode string, timeoutSec, reconnectDelaySec, expectTools int, expectVersion string, extraArgs []string) (*mcpClient, sessionInfo, any, error) {
	retryDelay := time.Duration(reconnectDelaySec) * time.Second
	if retryDelay < 0 {
		retryDelay = 0
	}
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = sessionRequestTimeout
	}
	deadline := time.Now().Add(timeout)
	attempt := 0
	var lastErr error

	for {
		if retryDelay > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			sleepFor := retryDelay
			if sleepFor > remaining {
				sleepFor = remaining
			}
			if attempt == 0 {
				fmt.Printf("  waiting %s before reconnect verification\n", sleepFor)
			} else {
				fmt.Printf("  waiting %s before reconnect retry\n", sleepFor)
			}
			time.Sleep(sleepFor)
		}
		attempt++
		if attempt > 1 {
			fmt.Printf("  reconnect attempt %d\n", attempt)
		}

		verifyClient, info, err := tryInitSession(binary, cwd, envMode, extraArgs, expectTools, expectVersion)
		if err == nil {
			_, healthPayload, healthErr := callTool(verifyClient, "sessions", map[string]any{"action": "health"}, sessionRequestTimeout)
			if healthErr == nil {
				return verifyClient, info, healthPayload, nil
			}
			verifyClient.close()
			err = fmt.Errorf("sessions(action=health): %w", healthErr)
		}
		lastErr = err

		if !time.Now().Before(deadline) {
			break
		}
		if retryDelay <= 0 {
			break
		}
	}

	if lastErr == nil {
		lastErr = errors.New("reconnect verification did not run")
	}
	return nil, sessionInfo{}, nil, lastErr
}

func waitForInstallBinaryReplacement(binary string, before fileFingerprint, timeoutSec, pollDelaySec int) error {
	pollDelay := time.Duration(pollDelaySec) * time.Second
	if pollDelay <= 0 {
		pollDelay = time.Second
	}
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = sessionRequestTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		current, err := fingerprintFile(binary)
		if err == nil && current.SHA256 != before.SHA256 {
			fmt.Printf("  installed binary changed: %s -> %s\n", before.SHA256[:12], current.SHA256[:12])
			return nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return fmt.Errorf("installed binary did not become readable before timeout: %w", err)
			}
			return fmt.Errorf("installed binary did not change before timeout")
		}

		remaining := time.Until(deadline)
		sleepFor := pollDelay
		if sleepFor > remaining {
			sleepFor = remaining
		}
		fmt.Printf("  waiting %s for installed binary replacement\n", sleepFor)
		time.Sleep(sleepFor)
	}
}

func fingerprintFile(path string) (fileFingerprint, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fileFingerprint{}, err
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fileFingerprint{}, err
	}

	return fileFingerprint{
		Size:    stat.Size(),
		ModTime: stat.ModTime(),
		SHA256:  fmt.Sprintf("%x", hash.Sum(nil)),
	}, nil
}

func normalizeInstallValidation(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", installValidationReplacement:
		return installValidationReplacement, nil
	case installValidationActivePointer, "active_pointer", "successor":
		return installValidationActivePointer, nil
	default:
		return "", fmt.Errorf("unknown -install-validation %q (use %s or %s)", raw, installValidationReplacement, installValidationActivePointer)
	}
}

func readInstallActivePointer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("active pointer file is empty")
	}
	return value, nil
}

func waitForInstallActivePointerUpdate(path, before string, timeoutSec, pollDelaySec int) (string, error) {
	pollDelay := time.Duration(pollDelaySec) * time.Second
	if pollDelay <= 0 {
		pollDelay = time.Second
	}
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = sessionRequestTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		current, err := readInstallActivePointer(path)
		if err == nil && current != before {
			fmt.Printf("  active pointer changed: %q -> %q\n", before, current)
			return current, nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return "", fmt.Errorf("active pointer did not become readable before timeout: %w", err)
			}
			return "", fmt.Errorf("active pointer did not change before timeout")
		}

		remaining := time.Until(deadline)
		sleepFor := pollDelay
		if sleepFor > remaining {
			sleepFor = remaining
		}
		fmt.Printf("  waiting %s for active pointer update\n", sleepFor)
		time.Sleep(sleepFor)
	}
}

func effectiveInstallReconnectDelaySec(requested int, explicit bool, upgradePayload any) int {
	if explicit {
		return requested
	}
	if requested < postExitInstallReconnectDelaySec && isPostExitInstallScheduled(upgradePayload) {
		fmt.Printf("  post-exit install scheduled; using %ds reconnect delay (override with -reconnect-delay)\n", postExitInstallReconnectDelaySec)
		return postExitInstallReconnectDelaySec
	}
	return requested
}

func isPostExitInstallScheduled(payload any) bool {
	obj, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	status, _ := obj["status"].(string)
	handoffError, _ := obj["handoff_error"].(string)
	message, _ := obj["message"].(string)
	if strings.EqualFold(status, "updated_deferred") {
		return true
	}
	text := strings.ToLower(status + " " + handoffError + " " + message)
	return strings.Contains(text, "post-exit install scheduled")
}

func shouldWaitForInstallReplacement(installValidation string, payload any, upgradeDisconnected bool) bool {
	if installValidation != installValidationReplacement {
		return false
	}
	return upgradeDisconnected || isPostExitInstallScheduled(payload)
}

func shouldWaitForInstallActivePointer(installValidation string) bool {
	return installValidation == installValidationActivePointer
}

func isExpectedUpgradeDisconnect(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "use of closed") ||
		strings.Contains(msg, "upstream restarted") ||
		strings.Contains(msg, "request lost during reconnect")
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	binary := flag.String("binary", "", "MCP server executable (required)")
	cwd := flag.String("cwd", ".", "working directory for subprocess")
	mode := flag.String("mode", "hold", "mode: hold, call, tool, resource, install, test, phase2, persist, kill-reconnect, compat")
	holdSec := flag.Int("hold", 300, "hold duration in seconds (hold mode)")
	watchSec := flag.Int("watch", 60, "watch duration in seconds after disconnect (persist mode)")
	ctlSocket := flag.String("ctl", "", "daemon control socket path (required for test/phase2/persist/kill-reconnect)")
	daemonFlag := flag.String("daemon-flag", "--muxcore-daemon", "flag to start server in daemon mode")
	envMode := flag.String("env-mode", "full", "environment mode: full (CC-style) or clean (Codex-style with selected smoke contract variables preserved)")
	timeoutSec := flag.Int("timeout", 120, "MCP request timeout in seconds, including initialize and tools/list")
	compatLevelFlag := flag.String("compat-level", "standard", "compat audit level: smoke, standard, lifecycle, or maximum")
	compatProfilesFlag := flag.String("compat-profiles", "generic,claude-code,codex", "comma-separated compat profiles: generic, claude-code, codex, fixture, openclaw-registry, hermes")
	compatReportFlag := flag.String("compat-report", "", "write compat audit JSON report to this path")
	reconnectDelaySec := flag.Int("reconnect-delay", defaultInstallReconnectDelaySec, "initial/retry delay in seconds for install reconnect verification")
	expectTools := flag.Int("expect-tools", 0, "expected tools/list count after session init (0 disables)")
	expectVersion := flag.String("expect-version", "", "expected MCP serverInfo.version after session init")
	method := flag.String("method", "", "JSON-RPC method for call mode")
	paramsJSON := flag.String("params", "{}", "JSON params for call mode")
	toolName := flag.String("tool", "", "MCP tool name for tool mode")
	argsJSON := flag.String("args", "{}", "JSON object arguments for tool mode")
	uri := flag.String("uri", "", "MCP resource URI for resource mode")
	source := flag.String("source", "", "local source binary for install mode")
	force := flag.Bool("force", false, "force upgrade apply in install mode")
	upgradeMode := flag.String("upgrade-mode", "auto", "upgrade mode for install: auto, hot_swap, or deferred")
	installValidation := flag.String("install-validation", installValidationReplacement, "install validation strategy: replacement or active-pointer")
	activeEngineFile := flag.String("active-engine-file", "", "active pointer file for install -install-validation active-pointer; defaults to MCPMUX_ACTIVE_ENGINE_FILE")
	cleanupBinary := flag.Bool("cleanup-binary-processes", false, "after one-shot mode completes, kill remaining processes with the same image name as -binary; use only with unique smoke binary copies")
	flag.Parse()
	sessionRequestTimeout = time.Duration(*timeoutSec) * time.Second
	reconnectDelayExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "reconnect-delay" {
			reconnectDelayExplicit = true
		}
	})

	if *binary == "" {
		fmt.Fprintln(os.Stderr, "error: -binary is required")
		fmt.Fprintf(os.Stderr, "\nUsage: mcp-launcher -binary <server> [options]\n\n")
		fmt.Fprintln(os.Stderr, "Modes:")
		fmt.Fprintln(os.Stderr, "  hold    Spawn server, hold session open for external testing (default)")
		fmt.Fprintln(os.Stderr, "  call    Call any JSON-RPC method after MCP initialize")
		fmt.Fprintln(os.Stderr, "  tool    Call any MCP tool by name with JSON arguments")
		fmt.Fprintln(os.Stderr, "  resource Read any MCP resource by URI")
		fmt.Fprintln(os.Stderr, "  install Install a local binary via upgrade tool, reconnect, and verify health")
		fmt.Fprintln(os.Stderr, "  test    Single-phase graceful-restart test")
		fmt.Fprintln(os.Stderr, "  phase2  Two-phase restart test (deadlock reproducer)")
		fmt.Fprintln(os.Stderr, "  persist Verify daemon stays alive across stdio disconnect + reconnect")
		fmt.Fprintln(os.Stderr, "  compat  Run profile-aware MCP stdio compatibility audit")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode hold")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode compat -compat-level standard")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./aimux-dev.exe -mode tool -tool sessions -args '{\"action\":\"health\"}'")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./aimux-dev.exe -mode resource -uri aimux://health")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./aimux-dev.exe -mode install -source ./aimux-dev-next.exe -force -expect-tools 27")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode test -ctl /tmp/my-ctl.sock")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode phase2 -ctl /tmp/my-ctl.sock")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode persist -ctl /tmp/my-ctl.sock -watch 60")
		os.Exit(1)
	}

	// Resolve binary to absolute path
	absBinary, err := filepath.Abs(*binary)
	if err == nil {
		if _, statErr := os.Stat(absBinary); statErr == nil {
			*binary = absBinary
		}
	}

	if (*mode == "test" || *mode == "phase2" || *mode == "persist" || *mode == "kill-reconnect") && *ctlSocket == "" {
		fmt.Fprintf(os.Stderr, "error: -ctl is required for %s mode\n", *mode)
		os.Exit(1)
	}

	extraArgs := flag.Args()

	exitCode := 0
	switch *mode {
	case "hold":
		runHold(*binary, *cwd, *envMode, *holdSec, *expectTools, *expectVersion, extraArgs)
	case "call":
		exitCode = runRawCall(*binary, *cwd, *envMode, *method, *paramsJSON, *timeoutSec, *expectTools, *expectVersion, extraArgs)
	case "tool":
		exitCode = runTool(*binary, *cwd, *envMode, *toolName, *argsJSON, *timeoutSec, *expectTools, *expectVersion, extraArgs)
	case "resource":
		exitCode = runResource(*binary, *cwd, *envMode, *uri, *timeoutSec, *expectTools, *expectVersion, extraArgs)
	case "install":
		exitCode = runInstall(*binary, *cwd, *envMode, *source, *upgradeMode, *installValidation, *activeEngineFile, *force, *timeoutSec, *reconnectDelaySec, reconnectDelayExplicit, *cleanupBinary, *expectTools, *expectVersion, extraArgs)
	case "test":
		runTest(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	case "phase2":
		runPhase2(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	case "persist":
		runPersist(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, *watchSec, extraArgs)
	case "kill-reconnect":
		runKillReconnect(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	case "compat":
		exitCode = runCompatMode(compatConfig{
			Binary:      *binary,
			CWD:         *cwd,
			EnvMode:     *envMode,
			Timeout:     time.Duration(*timeoutSec) * time.Second,
			CtlSocket:   *ctlSocket,
			Source:      *source,
			ExtraArgs:   extraArgs,
			LevelRaw:    *compatLevelFlag,
			ProfilesRaw: *compatProfilesFlag,
			ReportPath:  *compatReportFlag,
		})
	default:
		fmt.Fprintf(os.Stderr, "error: unknown mode %q (use hold, call, tool, resource, install, test, phase2, persist, kill-reconnect, or compat)\n", *mode)
		os.Exit(1)
	}
	if *cleanupBinary {
		if err := cleanupBinaryProcesses(*binary); err != nil {
			fmt.Fprintf(os.Stderr, "  WARN cleanup-binary-processes: %v\n", err)
		}
	}
	os.Exit(exitCode)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Avoid cutting in the middle of a multi-byte rune
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func envList() string {
	var sb strings.Builder
	for i, e := range os.Environ() {
		if i > 0 {
			sb.WriteString(", ")
		}
		k, _, _ := strings.Cut(e, "=")
		sb.WriteString(k)
	}
	return sb.String()
}
