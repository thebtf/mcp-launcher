package main

import (
	"fmt"
	"os"
	"strings"
)

type profileDefinition struct {
	ID                 profileID
	DisplayName        string
	Supported          bool
	Evidence           string
	UnsupportedMessage string
	EnvMode            string
	InjectClaudeDir    bool
}

func parseCompatProfiles(raw string) ([]profileID, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultCompatProfiles(), nil
	}
	var profiles []profileID
	seen := map[profileID]bool{}
	for _, part := range strings.Split(raw, ",") {
		id := profileID(strings.TrimSpace(part))
		if id == "" {
			continue
		}
		if _, ok := compatProfileDefinitions()[id]; !ok {
			return nil, errInvalidCompatProfile(string(id))
		}
		if !seen[id] {
			profiles = append(profiles, id)
			seen[id] = true
		}
	}
	if len(profiles) == 0 {
		return nil, errInvalidCompatProfile(raw)
	}
	return profiles, nil
}

func defaultCompatProfiles() []profileID {
	return []profileID{profileGeneric, profileClaudeCode, profileCodex}
}

func compatProfileDefinitions() map[profileID]profileDefinition {
	return map[profileID]profileDefinition{
		profileGeneric: {
			ID:          profileGeneric,
			DisplayName: "Generic MCP",
			Supported:   true,
			Evidence:    "MCP lifecycle and stdio transport specifications",
			EnvMode:     "full",
		},
		profileClaudeCode: {
			ID:              profileClaudeCode,
			DisplayName:     "Claude Code-style",
			Supported:       true,
			Evidence:        "Claude Code MCP docs: stdio launch plus CLAUDE_PROJECT_DIR host envelope",
			EnvMode:         "full",
			InjectClaudeDir: true,
		},
		profileCodex: {
			ID:          profileCodex,
			DisplayName: "Codex-style",
			Supported:   true,
			Evidence:    "Codex MCP docs: config.toml env/cwd/startup timeout host envelope",
			EnvMode:     "clean",
		},
		profileFixture: {
			ID:                 profileFixture,
			DisplayName:        "Fixture",
			Supported:          false,
			Evidence:           "Fixture profiles require captured traces supplied by the user",
			UnsupportedMessage: "fixture profile is reserved for captured trace fixtures; provide a fixture before expecting consumer claims",
		},
		profileOpenClawRegistry: {
			ID:                 profileOpenClawRegistry,
			DisplayName:        "OpenClaw registry",
			Supported:          false,
			Evidence:           "OpenClaw docs describe registry/bridge behavior, not a default Claude/Codex-equivalent stdio client profile",
			UnsupportedMessage: "openclaw-registry profile needs a scoped registry or bridge fixture before emulation is source-backed",
		},
		profileHermes: {
			ID:                 profileHermes,
			DisplayName:        "Hermes",
			Supported:          false,
			Evidence:           "No primary Hermes MCP consumer docs or trace were accepted in the 2026-06-16 research pass",
			UnsupportedMessage: "hermes profile is not implemented: needs primary MCP consumer docs or a captured stdio trace",
		},
	}
}

func envForProfile(profile profileDefinition, cfg compatConfig) []string {
	envMode := profile.EnvMode
	if profile.ID == profileGeneric && cfg.EnvMode != "" {
		envMode = cfg.EnvMode
	}
	env := envForMode(envMode)
	if profile.InjectClaudeDir {
		env = upsertEnv(env, "CLAUDE_PROJECT_DIR", cfg.CWD)
	}
	return env
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			next := append([]string{}, env...)
			next[i] = prefix + value
			return next
		}
	}
	return append(append([]string{}, env...), prefix+value)
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

type invalidCompatLevelError string

func errInvalidCompatLevel(raw string) error {
	return invalidCompatLevelError(raw)
}

func (e invalidCompatLevelError) Error() string {
	return fmt.Sprintf("invalid compat level %q (use smoke, standard, lifecycle, or maximum)", string(e))
}

type invalidCompatProfileError string

func errInvalidCompatProfile(raw string) error {
	return invalidCompatProfileError(raw)
}

func (e invalidCompatProfileError) Error() string {
	return fmt.Sprintf("invalid compat profile %q (use generic, claude-code, codex, fixture, openclaw-registry, or hermes)", string(e))
}

func parentEnvContains(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}
