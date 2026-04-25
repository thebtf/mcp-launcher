// mcp-launcher emulates how Claude Code and Codex spawn MCP servers as stdio
// subprocesses. Use it to test graceful restart, hot-swap handoff, session
// lifecycle, and MCP protocol compliance without a live AI client.
//
// Modes:
//
//	hold   — spawn server, hold session open for external testing (default)
//	test   — single-phase: daemon + owner + graceful-restart + verify
//	phase2 — two-phase: bootstrap + restart on successor (deadlock reproducer)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

	mu        sync.Mutex
	nextID    int
	pending   map[int]chan json.RawMessage
	notifCh   chan jsonRPCMessage
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newMCPClient(binary, cwd, envMode string) (*mcpClient, error) {
	cmd := exec.Command(binary)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr

	switch envMode {
	case "full":
		// CC behavior: pass full parent env
		cmd.Env = os.Environ()
	case "clean":
		// Codex behavior: platform allow-list only
		cmd.Env = cleanEnv()
	default:
		cmd.Env = os.Environ()
	}

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

func (c *mcpClient) readLoop() {
	defer c.closeOnce.Do(func() { close(c.closeCh) })
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg jsonRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
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
	c.cmd.Wait()
}

func (c *mcpClient) pid() int { return c.cmd.Process.Pid }

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

func initSession(binary, cwd, envMode string) *mcpClient {
	fmt.Printf("  spawn %s (cwd=%s, env=%s)\n", binary, cwd, envMode)
	client, err := newMCPClient(binary, cwd, envMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL spawn: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  pid=%d\n", client.pid())

	resp, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "mcp-launcher", "version": "1.0.0"},
		"capabilities":    map[string]any{"roots": map[string]any{}},
	}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL initialize: %v\n", err)
		client.close()
		os.Exit(1)
	}
	fmt.Printf("  initialize: %s\n", truncate(string(resp), 120))

	client.notify("notifications/initialized", map[string]any{})

	resp, err = client.call("tools/list", map[string]any{}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL tools/list: %v\n", err)
		client.close()
		os.Exit(1)
	}
	var tr struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(resp, &tr)
	fmt.Printf("  tools: %d\n", len(tr.Result.Tools))

	return client
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

func runHold(binary, cwd, envMode string, holdSec int) {
	fmt.Println("[hold] Starting MCP session...")
	client := initSession(binary, cwd, envMode)
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

func runTest(binary, cwd, envMode, ctlSocket, daemonFlag string) {
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

	client := initSession(binary, cwd, envMode)
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

func runPhase2(binary, cwd, envMode, ctlSocket, daemonFlag string) {
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

	client := initSession(binary, cwd, envMode)
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

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	binary := flag.String("binary", "", "MCP server executable (required)")
	cwd := flag.String("cwd", ".", "working directory for subprocess")
	mode := flag.String("mode", "hold", "mode: hold, test, phase2")
	holdSec := flag.Int("hold", 300, "hold duration in seconds (hold mode)")
	ctlSocket := flag.String("ctl", "", "daemon control socket path (required for test/phase2)")
	daemonFlag := flag.String("daemon-flag", "--muxcore-daemon", "flag to start server in daemon mode")
	envMode := flag.String("env-mode", "full", "environment mode: full (CC-style) or clean (Codex-style)")
	flag.Parse()

	if *binary == "" {
		fmt.Fprintln(os.Stderr, "error: -binary is required")
		fmt.Fprintf(os.Stderr, "\nUsage: mcp-launcher -binary <server> [options]\n\n")
		fmt.Fprintln(os.Stderr, "Modes:")
		fmt.Fprintln(os.Stderr, "  hold    Spawn server, hold session open for external testing (default)")
		fmt.Fprintln(os.Stderr, "  test    Single-phase graceful-restart test")
		fmt.Fprintln(os.Stderr, "  phase2  Two-phase restart test (deadlock reproducer)")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode hold")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode test -ctl /tmp/my-ctl.sock")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode phase2 -ctl /tmp/my-ctl.sock")
		os.Exit(1)
	}

	// Resolve binary to absolute path
	absBinary, err := filepath.Abs(*binary)
	if err == nil {
		if _, statErr := os.Stat(absBinary); statErr == nil {
			*binary = absBinary
		}
	}

	if (*mode == "test" || *mode == "phase2") && *ctlSocket == "" {
		fmt.Fprintf(os.Stderr, "error: -ctl is required for %s mode\n", *mode)
		os.Exit(1)
	}

	switch *mode {
	case "hold":
		runHold(*binary, *cwd, *envMode, *holdSec)
	case "test":
		runTest(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag)
	case "phase2":
		runPhase2(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown mode %q (use hold, test, or phase2)\n", *mode)
		os.Exit(1)
	}
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
