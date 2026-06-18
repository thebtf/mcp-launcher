package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveInstallReconnectDelay(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		explicit  bool
		payload   any
		want      int
	}{
		{
			name:      "uses post-exit grace for default",
			requested: defaultInstallReconnectDelaySec,
			payload: map[string]any{
				"status":        "updated_deferred",
				"handoff_error": "post-exit install scheduled",
			},
			want: postExitInstallReconnectDelaySec,
		},
		{
			name:      "uses post-exit grace for deferred status",
			requested: defaultInstallReconnectDelaySec,
			payload: map[string]any{
				"status": "updated_deferred",
			},
			want: postExitInstallReconnectDelaySec,
		},
		{
			name:      "honors explicit non-default value",
			requested: 1,
			explicit:  true,
			payload: map[string]any{
				"status":        "updated_deferred",
				"handoff_error": "post-exit install scheduled",
			},
			want: 1,
		},
		{
			name:      "keeps default for ordinary payload",
			requested: defaultInstallReconnectDelaySec,
			payload: map[string]any{
				"status":  "updated",
				"message": "Binary updated.",
			},
			want: defaultInstallReconnectDelaySec,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveInstallReconnectDelaySec(tt.requested, tt.explicit, tt.payload)
			if got != tt.want {
				t.Fatalf("effective delay = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsPostExitInstallScheduled(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    bool
	}{
		{
			name: "deferred status",
			payload: map[string]any{
				"status": "updated_deferred",
			},
			want: true,
		},
		{
			name: "handoff error phrase",
			payload: map[string]any{
				"handoff_error": "post-exit install scheduled",
			},
			want: true,
		},
		{
			name: "message phrase mixed case",
			payload: map[string]any{
				"message": "Post-Exit Install Scheduled",
			},
			want: true,
		},
		{
			name: "ordinary update",
			payload: map[string]any{
				"status": "updated",
			},
			want: false,
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    false,
		},
		{
			name:    "non-map payload",
			payload: "updated_deferred",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPostExitInstallScheduled(tt.payload)
			if got != tt.want {
				t.Fatalf("isPostExitInstallScheduled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldWaitForInstallReplacement(t *testing.T) {
	tests := []struct {
		name              string
		installValidation string
		payload           any
		upgradeErr        error
		want              bool
	}{
		{
			name:              "replacement validation waits when payload reports post-exit install",
			installValidation: installValidationReplacement,
			payload: map[string]any{
				"status": "updated_deferred",
			},
			want: true,
		},
		{
			name:              "replacement validation waits on expected disconnect before payload",
			installValidation: installValidationReplacement,
			upgradeErr:        errors.New("connection closed while waiting for tools/call"),
			want:              true,
		},
		{
			name:              "replacement validation still waits for post-exit replacement with explicit reconnect delay",
			installValidation: installValidationReplacement,
			payload: map[string]any{
				"status": "updated_deferred",
			},
			upgradeErr: errors.New("connection closed while waiting for tools/call"),
			want:       true,
		},
		{
			name:              "active pointer validation skips installed binary replacement wait",
			installValidation: installValidationActivePointer,
			payload: map[string]any{
				"status": "updated_deferred",
			},
			upgradeErr: errors.New("upstream restarted, request lost during reconnect"),
			want:       false,
		},
		{
			name:              "ordinary payload does not wait",
			installValidation: installValidationReplacement,
			payload: map[string]any{
				"status": "updated",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldWaitForInstallReplacement(tt.installValidation, tt.payload, isExpectedUpgradeDisconnect(tt.upgradeErr))
			if got != tt.want {
				t.Fatalf("shouldWaitForInstallReplacement = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeInstallValidation(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "", want: installValidationReplacement},
		{raw: "replacement", want: installValidationReplacement},
		{raw: "active-pointer", want: installValidationActivePointer},
		{raw: "active_pointer", want: installValidationActivePointer},
		{raw: "successor", want: installValidationActivePointer},
		{raw: "other", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := normalizeInstallValidation(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeInstallValidation(%q) succeeded, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeInstallValidation(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeInstallValidation(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestWaitForInstallActivePointerUpdateDetectsChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.txt")
	if err := os.WriteFile(path, []byte("next-binary"), 0o644); err != nil {
		t.Fatalf("write active pointer: %v", err)
	}

	got, err := waitForInstallActivePointerUpdate(path, "current-binary", 1, 0)
	if err != nil {
		t.Fatalf("waitForInstallActivePointerUpdate: %v", err)
	}
	if got != "next-binary" {
		t.Fatalf("active pointer = %q, want next-binary", got)
	}
}

func TestFingerprintFileDetectsContentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary.exe")
	if err := os.WriteFile(path, []byte("current"), 0o644); err != nil {
		t.Fatalf("write current content: %v", err)
	}
	before, err := fingerprintFile(path)
	if err != nil {
		t.Fatalf("fingerprint current content: %v", err)
	}

	if err := os.WriteFile(path, []byte("next"), 0o644); err != nil {
		t.Fatalf("write next content: %v", err)
	}
	after, err := fingerprintFile(path)
	if err != nil {
		t.Fatalf("fingerprint next content: %v", err)
	}

	if before.SHA256 == after.SHA256 {
		t.Fatalf("fingerprint hash did not change: %s", before.SHA256)
	}
}
