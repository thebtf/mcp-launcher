package main

import "testing"

func TestCleanEnvPreservesAimuxStdinEOFPolicy(t *testing.T) {
	t.Setenv("AIMUX_STDIN_EOF_POLICY", "eager")

	env := cleanEnv()

	if !envContains(env, "AIMUX_STDIN_EOF_POLICY=eager") {
		t.Fatalf("cleanEnv() did not preserve AIMUX_STDIN_EOF_POLICY=eager; env=%v", env)
	}
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
