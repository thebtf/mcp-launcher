package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func writeCompatConsole(w io.Writer, report compatReport) {
	fmt.Fprintf(w, "[compat] overall=%s level=%s profiles=%d checks=%d\n", report.OverallStatus, report.Request.Level, len(report.Profiles), len(report.Checks))
	for _, profile := range report.Profiles {
		fmt.Fprintf(w, "  %s: %s (pass=%d warn=%d fail=%d blocked=%d unsupported=%d)\n",
			profile.Profile,
			profile.Status,
			profile.Counts.Pass,
			profile.Counts.Warn,
			profile.Counts.Fail,
			profile.Counts.Blocked,
			profile.Counts.Unsupported,
		)
		if profile.FirstRemediation != "" {
			fmt.Fprintf(w, "    remediation: %s\n", profile.FirstRemediation)
		}
	}
}

func writeCompatJSONReport(path string, report compatReport) error {
	data, err := json.MarshalIndent(redactCompatReport(report), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func redactCompatReport(report compatReport) compatReport {
	report.Target.Binary = redactValue("binary", report.Target.Binary)
	report.Target.CWD = redactValue("cwd", report.Target.CWD)
	for i, arg := range report.Target.Args {
		report.Target.Args[i] = redactValue("arg", arg)
	}
	report.Checks = redactCompatResults(report.Checks)
	return report
}

func redactCompatResults(results []compatCheckResult) []compatCheckResult {
	out := make([]compatCheckResult, len(results))
	for i, result := range results {
		out[i] = result
		if result.Evidence == nil {
			continue
		}
		out[i].Evidence = map[string]string{}
		for key, value := range result.Evidence {
			out[i].Evidence[key] = redactValue(key, value)
		}
	}
	return out
}

func redactValue(key, value string) string {
	upperKey := strings.ToUpper(key)
	upperValue := strings.ToUpper(value)
	for _, token := range []string{"TOKEN", "SECRET", "PASSWORD", "AUTH", "HEADER"} {
		if strings.Contains(upperKey, token) {
			return "[REDACTED]"
		}
	}
	if upperKey == "KEY" || strings.HasSuffix(upperKey, "_KEY") || strings.Contains(upperKey, "KEY_") {
		return "[REDACTED]"
	}
	if strings.HasPrefix(upperValue, "BEARER ") {
		return "Bearer [REDACTED]"
	}
	if containsSensitiveAssignment(upperValue) {
		return "[REDACTED]"
	}
	return value
}

func containsSensitiveAssignment(value string) bool {
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "AUTH", "HEADER", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY"} {
		if strings.Contains(value, marker+"=") || strings.Contains(value, marker+":") {
			return true
		}
	}
	return strings.HasPrefix(value, "KEY=") ||
		strings.Contains(value, "_KEY=") ||
		strings.Contains(value, "-KEY=") ||
		strings.Contains(value, ".KEY=")
}
