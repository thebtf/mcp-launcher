//go:build critical

// @critical
// @category: smoke
// @features: [CLI-HELP]
// @dev_stand: optional
package critical

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCLIHelpListsDocumentedModesAndFlags(t *testing.T) {
	cmd := exec.Command("go", "run", "../..", "-h")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run help failed: %v\n%s", err, out)
	}

	help := string(out)
	for _, want := range []string{
		"-binary string",
		"-mode string",
		"hold, call, tool, resource, install, test, phase2, persist, kill-reconnect",
		"daemon control socket path (required for test/phase2/persist/kill-reconnect)",
		"-cleanup-binary-processes",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help output missing %q\n%s", want, help)
		}
	}
}
