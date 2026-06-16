package main

import (
	"errors"
	"testing"
)

type fakeCompatRunner struct {
	err          error
	invalidLines int
	toolCount    int
}

func (f fakeCompatRunner) Init(profile profileDefinition, cfg compatConfig) (compatSessionObservation, error) {
	if f.err != nil {
		return compatSessionObservation{}, f.err
	}
	return compatSessionObservation{
		Info:               sessionInfo{ToolCount: f.toolCount, ServerName: "fake", ServerVersion: "test"},
		InvalidStdoutLines: f.invalidLines,
		CloseElapsedMS:     1,
		Evidence:           map[string]string{"profile": string(profile.ID), "SECRET_TOKEN": "abc123"},
	}, nil
}

func TestCompatRunnerPassFailBlockedUnsupported(t *testing.T) {
	cfg := compatConfig{
		Binary:   "/fake/server",
		CWD:      ".",
		Level:    compatLifecycle,
		Profiles: []profileID{profileGeneric, profileHermes},
	}
	report := runCompatAuditWithCatalog(cfg, defaultCompatCatalog(), fakeCompatRunner{toolCount: 2})
	if report.OverallStatus != compatBlocked {
		t.Fatalf("overall status = %s, want BLOCKED; report=%+v", report.OverallStatus, report)
	}
	if len(report.Profiles) != 2 {
		t.Fatalf("profiles len = %d, want 2", len(report.Profiles))
	}
	if report.Profiles[0].Profile != profileGeneric || report.Profiles[0].Counts.Blocked != 1 {
		t.Fatalf("generic profile verdict did not record blocked lifecycle check: %+v", report.Profiles[0])
	}
	if report.Profiles[1].Profile != profileHermes || report.Profiles[1].Status != compatUnsupported {
		t.Fatalf("hermes profile verdict = %+v, want unsupported", report.Profiles[1])
	}
}

func TestCompatRunnerFailureBeatsOtherStatuses(t *testing.T) {
	cfg := compatConfig{
		Binary:   "/fake/server",
		CWD:      ".",
		Level:    compatSmoke,
		Profiles: []profileID{profileGeneric},
	}
	report := runCompatAuditWithCatalog(cfg, defaultCompatCatalog(), fakeCompatRunner{err: errors.New("boom")})
	if report.OverallStatus != compatFail {
		t.Fatalf("overall status = %s, want FAIL", report.OverallStatus)
	}
	if report.Checks[0].Remediation == "" {
		t.Fatalf("failed check missing remediation: %+v", report.Checks[0])
	}
}

func TestCompatProfileMatrixSelectedProfiles(t *testing.T) {
	cfg := compatConfig{
		Binary:   "/fake/server",
		CWD:      ".",
		Level:    compatStandard,
		Profiles: []profileID{profileGeneric, profileCodex},
	}
	report := runCompatAuditWithCatalog(cfg, defaultCompatCatalog(), fakeCompatRunner{toolCount: 0})
	if len(report.Profiles) != 2 {
		t.Fatalf("profile count = %d, want 2", len(report.Profiles))
	}
	if report.Profiles[0].Profile != profileGeneric || report.Profiles[1].Profile != profileCodex {
		t.Fatalf("profile order drifted: %+v", report.Profiles)
	}
	if report.Profiles[0].Status != compatPass || report.Profiles[1].Status != compatPass {
		t.Fatalf("profile statuses = %+v, want both PASS", report.Profiles)
	}
}

func TestCompatUnsupportedProfileDoesNotLaunch(t *testing.T) {
	cfg := compatConfig{
		Binary:   "/fake/server",
		CWD:      ".",
		Level:    compatStandard,
		Profiles: []profileID{profileHermes},
	}
	report := runCompatAuditWithCatalog(cfg, defaultCompatCatalog(), fakeCompatRunner{err: errors.New("must not launch")})
	if report.OverallStatus != compatUnsupported {
		t.Fatalf("overall status = %s, want UNSUPPORTED", report.OverallStatus)
	}
	if got := report.Checks[0].Summary; got == "" {
		t.Fatalf("unsupported result missing summary")
	}
}
