package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type processCompatSessionRunner struct{}

func runCompatMode(cfg compatConfig) int {
	report, err := runCompatAudit(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compat: %v\n", err)
		return 1
	}
	writeCompatConsole(os.Stdout, report)
	if cfg.ReportPath != "" {
		if err := writeCompatJSONReport(cfg.ReportPath, report); err != nil {
			fmt.Fprintf(os.Stderr, "compat report: %v\n", err)
			return 1
		}
	}
	return compatExitCode(report.OverallStatus)
}

func runCompatAudit(cfg compatConfig) (compatReport, error) {
	level, err := parseCompatLevel(cfg.LevelRaw)
	if err != nil {
		return compatReport{}, err
	}
	profiles, err := parseCompatProfiles(cfg.ProfilesRaw)
	if err != nil {
		return compatReport{}, err
	}
	cfg.Level = level
	cfg.Profiles = profiles
	if cfg.Timeout <= 0 {
		cfg.Timeout = sessionRequestTimeout
	}
	return runCompatAuditWithCatalog(cfg, defaultCompatCatalog(), processCompatSessionRunner{}), nil
}

func runCompatAuditWithCatalog(cfg compatConfig, catalog []compatCheckDefinition, runner compatSessionRunner) compatReport {
	defs := compatProfileDefinitions()
	var results []compatCheckResult
	for _, profile := range cfg.Profiles {
		def := defs[profile]
		if !def.Supported {
			results = append(results, unsupportedProfileResult(def))
			continue
		}
		checks := checksFor(cfg.Level, profile, catalog)
		if len(checks) == 0 {
			results = append(results, compatCheckResult{
				ID:          "PROFILE-EVIDENCE-001",
				Profile:     profile,
				Tier:        cfg.Level,
				Status:      compatUnsupported,
				Severity:    "info",
				Summary:     "no checks apply to selected profile and level",
				Remediation: "select a different profile or level with applicable checks",
				Evidence:    map[string]string{"profile_evidence": def.Evidence},
			})
			continue
		}
		for _, check := range checks {
			results = append(results, check.Run(compatCheckContext{
				Config:  cfg,
				Profile: def,
				Check:   check,
				Runner:  runner,
			}))
		}
	}
	return buildCompatReport(cfg, results)
}

func unsupportedProfileResult(profile profileDefinition) compatCheckResult {
	return compatCheckResult{
		ID:          "PROFILE-EVIDENCE-001",
		Profile:     profile.ID,
		Tier:        compatSmoke,
		Status:      compatUnsupported,
		Severity:    "info",
		Summary:     profile.UnsupportedMessage,
		Remediation: "add primary documentation or a captured fixture trace before claiming this profile",
		Evidence: map[string]string{
			"profile_evidence": profile.Evidence,
		},
	}
}

func buildCompatReport(cfg compatConfig, results []compatCheckResult) compatReport {
	profileResults := map[profileID][]compatCheckResult{}
	for _, result := range results {
		profileResults[result.Profile] = append(profileResults[result.Profile], result)
	}
	var profileVerdicts []compatProfileVerdict
	var overallStatuses []compatStatus
	for _, profile := range cfg.Profiles {
		resultsForProfile := profileResults[profile]
		var statuses []compatStatus
		for _, result := range resultsForProfile {
			statuses = append(statuses, result.Status)
		}
		status := aggregateStatus(statuses)
		overallStatuses = append(overallStatuses, status)
		profileVerdicts = append(profileVerdicts, compatProfileVerdict{
			Profile:          profile,
			Status:           status,
			Counts:           countStatuses(resultsForProfile),
			FirstRemediation: firstRemediation(resultsForProfile),
		})
	}
	return compatReport{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Target: compatTargetReport{
			Binary: cfg.Binary,
			CWD:    cfg.CWD,
			Args:   append([]string{}, cfg.ExtraArgs...),
		},
		Request: compatRequestReport{
			Level:    cfg.Level,
			Profiles: append([]profileID{}, cfg.Profiles...),
		},
		OverallStatus: aggregateStatus(overallStatuses),
		Profiles:      profileVerdicts,
		Checks:        redactCompatResults(results),
	}
}

func (processCompatSessionRunner) Init(profile profileDefinition, cfg compatConfig) (compatSessionObservation, error) {
	env := envForProfile(profile, cfg)
	client, err := newMCPClientWithEnv(cfg.Binary, cfg.CWD, env, cfg.ExtraArgs)
	if err != nil {
		return compatSessionObservation{}, fmt.Errorf("spawn: %w", err)
	}
	info := sessionInfo{}
	resp, err := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": clientInfoName(profile.ID), "version": "1.0.0"},
		"capabilities":    map[string]any{"roots": map[string]any{}},
	}, cfg.Timeout)
	if err != nil {
		client.close()
		return compatSessionObservation{}, fmt.Errorf("initialize: %w", err)
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
	client.notify("notifications/initialized", map[string]any{})
	resp, err = client.call("tools/list", map[string]any{}, cfg.Timeout)
	if err != nil {
		client.close()
		return compatSessionObservation{}, fmt.Errorf("tools/list: %w", err)
	}
	var tr struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(resp, &tr)
	info.ToolCount = len(tr.Result.Tools)
	invalid := client.invalidStdoutCount()
	startClose := time.Now()
	client.close()
	closeElapsed := time.Since(startClose).Milliseconds()
	evidence := map[string]string{
		"server_name":    info.ServerName,
		"server_version": info.ServerVersion,
		"tool_count":     fmt.Sprint(info.ToolCount),
		"profile":        string(profile.ID),
		"profile_source": profile.Evidence,
	}
	if profile.InjectClaudeDir {
		if value, ok := envValue(env, "CLAUDE_PROJECT_DIR"); ok {
			evidence["CLAUDE_PROJECT_DIR"] = value
		}
	}
	if profile.ID == profileCodex {
		evidence["env_mode"] = "clean"
	}
	return compatSessionObservation{
		Info:               info,
		InvalidStdoutLines: invalid,
		CloseElapsedMS:     closeElapsed,
		Evidence:           evidence,
	}, nil
}

func clientInfoName(profile profileID) string {
	switch profile {
	case profileClaudeCode:
		return "claude-code"
	case profileCodex:
		return "codex"
	default:
		return "mcp-launcher"
	}
}

func compatExitCode(status compatStatus) int {
	switch status {
	case compatFail:
		return 1
	case compatBlocked, compatUnsupported:
		return 2
	default:
		return 0
	}
}
