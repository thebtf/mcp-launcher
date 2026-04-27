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
	"errors"
	"flag"
	"fmt"
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

	mu        sync.Mutex
	nextID    int
	pending   map[int]chan json.RawMessage
	notifCh   chan jsonRPCMessage
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newMCPClient(binary, cwd, envMode string, extraArgs []string) (*mcpClient, error) {
	cmd := exec.Command(binary, extraArgs...)
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

func initSession(binary, cwd, envMode string, extraArgs []string) *mcpClient {
	fmt.Printf("  spawn %s %v (cwd=%s, env=%s)\n", binary, extraArgs, cwd, envMode)
	client, err := newMCPClient(binary, cwd, envMode, extraArgs)
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

func runHold(binary, cwd, envMode string, holdSec int, extraArgs []string) {
	fmt.Println("[hold] Starting MCP session...")
	client := initSession(binary, cwd, envMode, extraArgs)
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

	client := initSession(binary, cwd, envMode, extraArgs)
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

	client := initSession(binary, cwd, envMode, extraArgs)
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
	clientA := initSession(binary, cwd, envMode, extraArgs)

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
	clientB := initSession(binary, cwd, envMode, extraArgs)
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
	clientA := initSession(binary, cwd, envMode, extraArgs)
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
	clientB := initSession(binary, cwd, envMode, extraArgs)
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

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	binary := flag.String("binary", "", "MCP server executable (required)")
	cwd := flag.String("cwd", ".", "working directory for subprocess")
	mode := flag.String("mode", "hold", "mode: hold, test, phase2, persist, kill-reconnect")
	holdSec := flag.Int("hold", 300, "hold duration in seconds (hold mode)")
	watchSec := flag.Int("watch", 60, "watch duration in seconds after disconnect (persist mode)")
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
		fmt.Fprintln(os.Stderr, "  persist Verify daemon stays alive across stdio disconnect + reconnect")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintln(os.Stderr, "  mcp-launcher -binary ./my-server -mode hold")
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

	switch *mode {
	case "hold":
		runHold(*binary, *cwd, *envMode, *holdSec, extraArgs)
	case "test":
		runTest(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	case "phase2":
		runPhase2(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	case "persist":
		runPersist(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, *watchSec, extraArgs)
	case "kill-reconnect":
		runKillReconnect(*binary, *cwd, *envMode, *ctlSocket, *daemonFlag, extraArgs)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown mode %q (use hold, test, phase2, or persist)\n", *mode)
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
