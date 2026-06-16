package main

import "testing"

func TestCompatCatalogStableIDs(t *testing.T) {
	catalog := defaultCompatCatalog()
	want := []string{
		"GEN-INIT-001",
		"GEN-TOOLS-001",
		"GEN-STDIO-001",
		"GEN-SHUTDOWN-001",
		"CLAUDE-ENV-001",
		"CODEX-ENV-001",
		"GEN-LIFECYCLE-CTL-001",
		"GEN-INSTALL-SOURCE-001",
	}
	if len(catalog) != len(want) {
		t.Fatalf("catalog len = %d, want %d", len(catalog), len(want))
	}
	for i, id := range want {
		if catalog[i].ID != id {
			t.Fatalf("catalog[%d].ID = %q, want %q", i, catalog[i].ID, id)
		}
		if catalog[i].Remediation == "" {
			t.Fatalf("%s missing remediation", catalog[i].ID)
		}
		if catalog[i].Severity == "" {
			t.Fatalf("%s missing severity", catalog[i].ID)
		}
	}
}

func TestCompatCatalogFiltering(t *testing.T) {
	catalog := defaultCompatCatalog()
	if got := checksFor(compatSmoke, profileGeneric, catalog); len(got) != 1 || got[0].ID != "GEN-INIT-001" {
		t.Fatalf("smoke generic checks = %+v", got)
	}
	if got := checksFor(compatStandard, profileCodex, catalog); len(got) != 1 || got[0].ID != "CODEX-ENV-001" {
		t.Fatalf("standard codex checks = %+v", got)
	}
	foundLifecycle := false
	for _, check := range checksFor(compatLifecycle, profileGeneric, catalog) {
		if check.ID == "GEN-LIFECYCLE-CTL-001" {
			foundLifecycle = true
		}
	}
	if !foundLifecycle {
		t.Fatalf("lifecycle generic checks did not include control socket check")
	}
}
