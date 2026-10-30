package helps

import (
	"encoding/json"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
)

func TestClaudeUsageQuotaHeaders(t *testing.T) {
	var usage claudeauth.OAuthUsage
	body := `{"five_hour":{"utilization":12.0,"resets_at":"2026-10-21T19:00:00.512+00:00"},` +
		`"seven_day":{"utilization":29.0,"resets_at":"2026-10-22T19:00:00Z"},"seven_day_opus":null}`
	if errUnmarshal := json.Unmarshal([]byte(body), &usage); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	headers := ClaudeUsageQuotaHeaders(&usage)
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1792695600",
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.29",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1792609200",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.12",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestCodexUsageQuotaHeaders(t *testing.T) {
	var usage CodexUsage
	body := `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,` +
		`"primary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":100,"reset_at":1792609200},` +
		`"secondary_window":{"used_percent":41.5,"limit_window_seconds":604800,"reset_at":1792695600}}}`
	if errUnmarshal := json.Unmarshal([]byte(body), &usage); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	headers := CodexUsageQuotaHeaders(&usage)
	want := map[string]string{
		"X-Codex-Plan-Type":                "plus",
		"X-Codex-Allowed":                  "true",
		"X-Codex-Limit-Reached":            "false",
		"X-Codex-Primary-Used-Percent":     "7",
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Reset-At":         "1792609200",
		"X-Codex-Secondary-Used-Percent":   "41.5",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       "1792695600",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}
