package main

import "testing"

func TestCleanEnvPreservesAimuxStdinEOFPolicy(t *testing.T) {
	t.Setenv("AIMUX_STDIN_EOF_POLICY", "eager")

	env := cleanEnv()

	if !envContains(env, "AIMUX_STDIN_EOF_POLICY=eager") {
		t.Fatalf("cleanEnv() did not preserve AIMUX_STDIN_EOF_POLICY=eager; env=%v", env)
	}
}

func TestCleanEnvPreservesAimuxIsolationContract(t *testing.T) {
	t.Setenv("AIMUX_ENGINE_NAME", "aimux-clean-smoke")
	t.Setenv("AIMUX_SESSION_STORE", "memory")
	t.Setenv("AIMUX_WARMUP", "false")

	env := cleanEnv()

	for _, want := range []string{
		"AIMUX_ENGINE_NAME=aimux-clean-smoke",
		"AIMUX_SESSION_STORE=memory",
		"AIMUX_WARMUP=false",
	} {
		if !envContains(env, want) {
			t.Fatalf("cleanEnv() did not preserve %s; env=%v", want, env)
		}
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
