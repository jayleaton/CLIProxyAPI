package helps

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
)

// ClaudeUsageQuotaHeaders expresses Claude OAuth usage windows as the
// anthropic-ratelimit-unified-* headers an inference response carries: utilization as a
// 0-1 fraction and the reset as Unix seconds. Windows without a parseable reset are omitted.
func ClaudeUsageQuotaHeaders(usage *claudeauth.OAuthUsage) http.Header {
	headers := http.Header{}
	if usage == nil {
		return headers
	}
	for name, window := range map[string]*claudeauth.OAuthUsageWindow{"5h": usage.FiveHour, "7d": usage.SevenDay} {
		if window == nil {
			continue
		}
		reset, errParse := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetsAt))
		if errParse != nil {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		headers.Set(prefix+"Reset", strconv.FormatInt(reset.Unix(), 10))
		if window.Utilization != nil {
			headers.Set(prefix+"Utilization", strconv.FormatFloat(*window.Utilization/100, 'f', -1, 64))
		}
	}
	return headers
}

// CodexUsageWindow is one window of the ChatGPT backend usage response.
type CodexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

// CodexUsage is the subset of the ChatGPT backend usage response that describes the
// credential-level rate limit windows.
type CodexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         *bool             `json:"allowed"`
		LimitReached    *bool             `json:"limit_reached"`
		PrimaryWindow   *CodexUsageWindow `json:"primary_window"`
		SecondaryWindow *CodexUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

// CodexUsageQuotaHeaders expresses Codex usage windows as the x-codex-* headers an inference
// response carries. Windows without a reset time are omitted.
func CodexUsageQuotaHeaders(usage *CodexUsage) http.Header {
	headers := http.Header{}
	if usage == nil {
		return headers
	}
	if plan := strings.TrimSpace(usage.PlanType); plan != "" {
		headers.Set("X-Codex-Plan-Type", plan)
	}
	limits := usage.RateLimit
	if limits == nil {
		return headers
	}
	if limits.Allowed != nil {
		headers.Set("X-Codex-Allowed", strconv.FormatBool(*limits.Allowed))
	}
	if limits.LimitReached != nil {
		headers.Set("X-Codex-Limit-Reached", strconv.FormatBool(*limits.LimitReached))
	}
	for name, window := range map[string]*CodexUsageWindow{"Primary": limits.PrimaryWindow, "Secondary": limits.SecondaryWindow} {
		if window == nil || window.ResetAt <= 0 {
			continue
		}
		prefix := "X-Codex-" + name + "-"
		headers.Set(prefix+"Reset-At", strconv.FormatInt(window.ResetAt, 10))
		if window.UsedPercent != nil {
			headers.Set(prefix+"Used-Percent", strconv.FormatFloat(*window.UsedPercent, 'f', -1, 64))
		}
		if window.LimitWindowSeconds > 0 {
			headers.Set(prefix+"Window-Minutes", strconv.FormatInt(window.LimitWindowSeconds/60, 10))
		}
	}
	return headers
}
