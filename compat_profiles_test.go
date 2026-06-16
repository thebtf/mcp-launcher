package main

import "testing"

func TestCompatDefaultProfiles(t *testing.T) {
	got, err := parseCompatProfiles("")
	if err != nil {
		t.Fatalf("parse default profiles: %v", err)
	}
	want := []profileID{profileGeneric, profileClaudeCode, profileCodex}
	if len(got) != len(want) {
		t.Fatalf("profiles len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profile[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCompatLevels(t *testing.T) {
	for _, level := range []string{"smoke", "standard", "lifecycle", "maximum"} {
		if _, err := parseCompatLevel(level); err != nil {
			t.Fatalf("level %q rejected: %v", level, err)
		}
	}
	if _, err := parseCompatLevel("full"); err == nil {
		t.Fatalf("invalid level accepted")
	}
	if !levelIncludes(compatMaximum, compatLifecycle) || levelIncludes(compatSmoke, compatStandard) {
		t.Fatalf("levelIncludes ordering is wrong")
	}
}

func TestCompatUnsupportedProfiles(t *testing.T) {
	profiles, err := parseCompatProfiles("hermes,fixture,openclaw-registry")
	if err != nil {
		t.Fatalf("parse unsupported profiles: %v", err)
	}
	defs := compatProfileDefinitions()
	for _, profile := range profiles {
		def := defs[profile]
		if def.Supported {
			t.Fatalf("%s unexpectedly supported", profile)
		}
		if def.UnsupportedMessage == "" || def.Evidence == "" {
			t.Fatalf("%s missing unsupported evidence metadata: %+v", profile, def)
		}
	}
}

func TestCompatInvalidProfile(t *testing.T) {
	if _, err := parseCompatProfiles("generic,nope"); err == nil {
		t.Fatalf("invalid profile accepted")
	}
}

func TestClaudeProfileInjectsProjectDir(t *testing.T) {
	cfg := compatConfig{CWD: `C:\work\project`}
	def := compatProfileDefinitions()[profileClaudeCode]
	env := envForProfile(def, cfg)
	got, ok := envValue(env, "CLAUDE_PROJECT_DIR")
	if !ok {
		t.Fatalf("CLAUDE_PROJECT_DIR was not injected")
	}
	if got != cfg.CWD {
		t.Fatalf("CLAUDE_PROJECT_DIR = %q, want %q", got, cfg.CWD)
	}
}
