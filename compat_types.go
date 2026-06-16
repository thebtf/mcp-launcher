package main

import "time"

type compatStatus string

const (
	compatPass        compatStatus = "PASS"
	compatWarn        compatStatus = "WARN"
	compatFail        compatStatus = "FAIL"
	compatBlocked     compatStatus = "BLOCKED"
	compatUnsupported compatStatus = "UNSUPPORTED"
)

type compatLevel string

const (
	compatSmoke     compatLevel = "smoke"
	compatStandard  compatLevel = "standard"
	compatLifecycle compatLevel = "lifecycle"
	compatMaximum   compatLevel = "maximum"
)

type profileID string

const (
	profileGeneric          profileID = "generic"
	profileClaudeCode       profileID = "claude-code"
	profileCodex            profileID = "codex"
	profileFixture          profileID = "fixture"
	profileOpenClawRegistry profileID = "openclaw-registry"
	profileHermes           profileID = "hermes"
)

type compatConfig struct {
	Binary      string
	CWD         string
	EnvMode     string
	Timeout     time.Duration
	CtlSocket   string
	Source      string
	ExtraArgs   []string
	Level       compatLevel
	Profiles    []profileID
	LevelRaw    string
	ProfilesRaw string
	ReportPath  string
}

type compatCounts struct {
	Pass        int `json:"pass"`
	Warn        int `json:"warn"`
	Fail        int `json:"fail"`
	Blocked     int `json:"blocked"`
	Unsupported int `json:"unsupported"`
}

type compatTargetReport struct {
	Binary string   `json:"binary"`
	CWD    string   `json:"cwd"`
	Args   []string `json:"args"`
}

type compatRequestReport struct {
	Level    compatLevel `json:"level"`
	Profiles []profileID `json:"profiles"`
}

type compatProfileVerdict struct {
	Profile          profileID    `json:"profile"`
	Status           compatStatus `json:"status"`
	Counts           compatCounts `json:"counts"`
	FirstRemediation string       `json:"first_remediation"`
}

type compatCheckResult struct {
	ID          string            `json:"id"`
	Profile     profileID         `json:"profile"`
	Tier        compatLevel       `json:"tier"`
	Status      compatStatus      `json:"status"`
	Severity    string            `json:"severity"`
	ElapsedMS   int64             `json:"elapsed_ms,omitempty"`
	Summary     string            `json:"summary"`
	Remediation string            `json:"remediation,omitempty"`
	Evidence    map[string]string `json:"evidence,omitempty"`
}

type compatReport struct {
	SchemaVersion int                    `json:"schema_version"`
	GeneratedAt   string                 `json:"generated_at"`
	Target        compatTargetReport     `json:"target"`
	Request       compatRequestReport    `json:"request"`
	OverallStatus compatStatus           `json:"overall_status"`
	Profiles      []compatProfileVerdict `json:"profiles"`
	Checks        []compatCheckResult    `json:"checks"`
}

func parseCompatLevel(raw string) (compatLevel, error) {
	switch compatLevel(raw) {
	case compatSmoke, compatStandard, compatLifecycle, compatMaximum:
		return compatLevel(raw), nil
	default:
		return "", errInvalidCompatLevel(raw)
	}
}

func levelIncludes(selected, check compatLevel) bool {
	order := map[compatLevel]int{
		compatSmoke:     1,
		compatStandard:  2,
		compatLifecycle: 3,
		compatMaximum:   4,
	}
	return order[selected] >= order[check]
}

func aggregateStatus(statuses []compatStatus) compatStatus {
	if len(statuses) == 0 {
		return compatUnsupported
	}
	best := compatPass
	bestRank := 0
	for _, status := range statuses {
		rank := statusRank(status)
		if rank > bestRank {
			best = status
			bestRank = rank
		}
	}
	return best
}

func statusRank(status compatStatus) int {
	switch status {
	case compatFail:
		return 5
	case compatBlocked:
		return 4
	case compatWarn:
		return 3
	case compatUnsupported:
		return 2
	case compatPass:
		return 1
	default:
		return 0
	}
}

func countStatuses(results []compatCheckResult) compatCounts {
	var counts compatCounts
	for _, result := range results {
		switch result.Status {
		case compatPass:
			counts.Pass++
		case compatWarn:
			counts.Warn++
		case compatFail:
			counts.Fail++
		case compatBlocked:
			counts.Blocked++
		case compatUnsupported:
			counts.Unsupported++
		}
	}
	return counts
}

func firstRemediation(results []compatCheckResult) string {
	for _, result := range results {
		if result.Status != compatPass && result.Remediation != "" {
			return result.Remediation
		}
	}
	return ""
}
