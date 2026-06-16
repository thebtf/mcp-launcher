package main

import (
	"fmt"
	"time"
)

type compatCheckDefinition struct {
	ID          string
	Title       string
	Tier        compatLevel
	Profiles    []profileID
	Severity    string
	Remediation string
	Run         func(compatCheckContext) compatCheckResult
}

type compatCheckContext struct {
	Config  compatConfig
	Profile profileDefinition
	Check   compatCheckDefinition
	Runner  compatSessionRunner
}

type compatSessionObservation struct {
	Info               sessionInfo
	InvalidStdoutLines int
	CloseElapsedMS     int64
	Evidence           map[string]string
}

type compatSessionRunner interface {
	Init(profile profileDefinition, cfg compatConfig) (compatSessionObservation, error)
}

func defaultCompatCatalog() []compatCheckDefinition {
	return []compatCheckDefinition{
		{
			ID:          "GEN-INIT-001",
			Title:       "MCP initialize flow",
			Tier:        compatSmoke,
			Profiles:    []profileID{profileGeneric},
			Severity:    "error",
			Remediation: "ensure the server responds to initialize, then accepts notifications/initialized and tools/list over stdout JSON-RPC",
			Run:         runCompatBootstrapCheck,
		},
		{
			ID:          "GEN-TOOLS-001",
			Title:       "MCP tools/list classification",
			Tier:        compatStandard,
			Profiles:    []profileID{profileGeneric},
			Severity:    "warning",
			Remediation: "return a valid tools/list result; an empty tool list is valid but must be encoded as JSON-RPC",
			Run:         runCompatToolsCheck,
		},
		{
			ID:          "GEN-STDIO-001",
			Title:       "stdio stdout cleanliness",
			Tier:        compatStandard,
			Profiles:    []profileID{profileGeneric},
			Severity:    "error",
			Remediation: "send diagnostics to stderr; stdout must contain only newline-delimited JSON-RPC messages",
			Run:         runCompatStdoutCheck,
		},
		{
			ID:          "GEN-SHUTDOWN-001",
			Title:       "bounded stdin-close shutdown",
			Tier:        compatStandard,
			Profiles:    []profileID{profileGeneric},
			Severity:    "warning",
			Remediation: "exit promptly after client stdin closes or make daemon behavior explicit through lifecycle modes",
			Run:         runCompatShutdownCheck,
		},
		{
			ID:          "CLAUDE-ENV-001",
			Title:       "Claude Code-style launch envelope",
			Tier:        compatStandard,
			Profiles:    []profileID{profileClaudeCode},
			Severity:    "warning",
			Remediation: "support a full inherited environment and tolerate CLAUDE_PROJECT_DIR pointing at the launch directory",
			Run:         runCompatBootstrapCheck,
		},
		{
			ID:          "CODEX-ENV-001",
			Title:       "Codex-style clean env and cwd envelope",
			Tier:        compatStandard,
			Profiles:    []profileID{profileCodex},
			Severity:    "warning",
			Remediation: "avoid depending on ambient env vars outside the configured allow-list; honor the configured cwd",
			Run:         runCompatBootstrapCheck,
		},
		{
			ID:          "GEN-LIFECYCLE-CTL-001",
			Title:       "daemon control lifecycle input",
			Tier:        compatLifecycle,
			Profiles:    []profileID{profileGeneric},
			Severity:    "warning",
			Remediation: "provide -ctl for lifecycle audits or run a lower compat level",
			Run:         runCompatControlSocketCheck,
		},
		{
			ID:          "GEN-INSTALL-SOURCE-001",
			Title:       "install reconnect source input",
			Tier:        compatMaximum,
			Profiles:    []profileID{profileGeneric},
			Severity:    "warning",
			Remediation: "provide -source for maximum install-reconnect coverage or run lifecycle/standard level",
			Run:         runCompatInstallSourceCheck,
		},
	}
}

func checksFor(level compatLevel, profile profileID, catalog []compatCheckDefinition) []compatCheckDefinition {
	var checks []compatCheckDefinition
	for _, check := range catalog {
		if !levelIncludes(level, check.Tier) {
			continue
		}
		if check.appliesTo(profile) {
			checks = append(checks, check)
		}
	}
	return checks
}

func (c compatCheckDefinition) appliesTo(profile profileID) bool {
	for _, candidate := range c.Profiles {
		if candidate == profile {
			return true
		}
	}
	return false
}

func runCompatBootstrapCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	obs, err := ctx.Runner.Init(ctx.Profile, ctx.Config)
	if err != nil {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("bootstrap failed: %v", err), map[string]string{
			"profile_evidence": ctx.Profile.Evidence,
		})
	}
	return ctx.result(compatPass, time.Since(start), fmt.Sprintf("initialize and tools/list succeeded for %s; tools=%d", ctx.Profile.ID, obs.Info.ToolCount), obs.Evidence)
}

func runCompatToolsCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	obs, err := ctx.Runner.Init(ctx.Profile, ctx.Config)
	if err != nil {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("tools/list bootstrap failed: %v", err), nil)
	}
	return ctx.result(compatPass, time.Since(start), fmt.Sprintf("tools/list returned %d tools", obs.Info.ToolCount), obs.Evidence)
}

func runCompatStdoutCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	obs, err := ctx.Runner.Init(ctx.Profile, ctx.Config)
	if err != nil {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("stdout cleanliness could not be evaluated: %v", err), nil)
	}
	if obs.InvalidStdoutLines > 0 {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("stdout contained %d non-JSON-RPC line(s)", obs.InvalidStdoutLines), obs.Evidence)
	}
	return ctx.result(compatPass, time.Since(start), "stdout contained only JSON-RPC messages observed by the launcher", obs.Evidence)
}

func runCompatShutdownCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	obs, err := ctx.Runner.Init(ctx.Profile, ctx.Config)
	if err != nil {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("shutdown check bootstrap failed: %v", err), nil)
	}
	if obs.CloseElapsedMS >= 4900 {
		return ctx.result(compatWarn, time.Since(start), fmt.Sprintf("client close took %dms and may have required kill escalation", obs.CloseElapsedMS), obs.Evidence)
	}
	return ctx.result(compatPass, time.Since(start), fmt.Sprintf("stdin-close shutdown completed in %dms", obs.CloseElapsedMS), obs.Evidence)
}

func runCompatControlSocketCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	if ctx.Config.CtlSocket == "" {
		return ctx.result(compatBlocked, time.Since(start), "lifecycle audit requires -ctl but no control socket path was supplied", map[string]string{"missing": "-ctl"})
	}
	resp, err := controlSend(ctx.Config.CtlSocket, "status", ctx.Config.Timeout)
	if err != nil {
		return ctx.result(compatFail, time.Since(start), fmt.Sprintf("control socket status failed: %v", err), map[string]string{"ctl": ctx.Config.CtlSocket})
	}
	return ctx.result(compatPass, time.Since(start), "control socket status responded", stringifyEvidence(resp))
}

func runCompatInstallSourceCheck(ctx compatCheckContext) compatCheckResult {
	start := time.Now()
	if ctx.Config.Source == "" {
		return ctx.result(compatBlocked, time.Since(start), "maximum audit includes install reconnect but -source was not supplied", map[string]string{"missing": "-source"})
	}
	return ctx.result(compatWarn, time.Since(start), "install source supplied; destructive upgrade execution remains in dedicated install mode for CR-001", map[string]string{"source": ctx.Config.Source})
}

func (ctx compatCheckContext) result(status compatStatus, elapsed time.Duration, summary string, evidence map[string]string) compatCheckResult {
	return compatCheckResult{
		ID:          ctx.Check.ID,
		Profile:     ctx.Profile.ID,
		Tier:        ctx.Check.Tier,
		Status:      status,
		Severity:    ctx.Check.Severity,
		ElapsedMS:   elapsed.Milliseconds(),
		Summary:     summary,
		Remediation: remediationForStatus(status, ctx.Check.Remediation),
		Evidence:    evidence,
	}
}

func remediationForStatus(status compatStatus, remediation string) string {
	if status == compatPass {
		return ""
	}
	return remediation
}

func stringifyEvidence(values map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range values {
		out[k] = fmt.Sprint(v)
	}
	return out
}
