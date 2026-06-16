package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompatReportJSONSchemaAndRedaction(t *testing.T) {
	report := compatReport{
		SchemaVersion: 1,
		GeneratedAt:   "2026-06-16T00:00:00Z",
		Target:        compatTargetReport{Binary: "server", CWD: ".", Args: []string{"--token=abc"}},
		Request:       compatRequestReport{Level: compatStandard, Profiles: []profileID{profileGeneric}},
		OverallStatus: compatPass,
		Profiles:      []compatProfileVerdict{{Profile: profileGeneric, Status: compatPass, Counts: compatCounts{Pass: 1}}},
		Checks: []compatCheckResult{{
			ID: "GEN-INIT-001", Profile: profileGeneric, Tier: compatSmoke, Status: compatPass, Severity: "error", Summary: "ok",
			Evidence: map[string]string{"API_TOKEN": "secret", "auth_header": "Bearer abc"},
		}},
	}
	path := filepath.Join(t.TempDir(), "compat-report.json")
	if err := writeCompatJSONReport(path, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var decoded compatReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, data)
	}
	if decoded.SchemaVersion != 1 || decoded.OverallStatus != compatPass || len(decoded.Profiles) != 1 || len(decoded.Checks) != 1 {
		t.Fatalf("decoded report missing required fields: %+v", decoded)
	}
	body := string(data)
	if strings.Contains(body, "secret") || strings.Contains(body, "Bearer abc") || strings.Contains(body, "--token=abc") {
		t.Fatalf("report leaked secret-like values:\n%s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("report did not contain redaction marker:\n%s", body)
	}
}

func TestCompatConsoleSummary(t *testing.T) {
	report := compatReport{
		Request:       compatRequestReport{Level: compatStandard},
		OverallStatus: compatBlocked,
		Profiles: []compatProfileVerdict{{
			Profile:          profileGeneric,
			Status:           compatBlocked,
			Counts:           compatCounts{Blocked: 1},
			FirstRemediation: "provide -ctl",
		}},
	}
	var out strings.Builder
	writeCompatConsole(&out, report)
	got := out.String()
	for _, want := range []string{"overall=BLOCKED", "generic: BLOCKED", "provide -ctl"} {
		if !strings.Contains(got, want) {
			t.Fatalf("console summary missing %q:\n%s", want, got)
		}
	}
}
