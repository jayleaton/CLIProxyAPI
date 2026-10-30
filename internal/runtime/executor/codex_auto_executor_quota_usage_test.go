package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The service registers CodexAutoExecutor, so the quota usage probe only reaches Codex
// credentials if the wrapper itself implements QuotaUsageFetcher.
func TestCodexAutoExecutorForwardsQuotaUsage(t *testing.T) {
	var registered any = NewCodexAutoExecutor(&config.Config{})
	fetcher, ok := registered.(cliproxyauth.QuotaUsageFetcher)
	if !ok {
		t.Fatal("CodexAutoExecutor does not implement QuotaUsageFetcher; idle Codex credentials are never probed")
	}
	apiKeyAuth := &cliproxyauth.Auth{ID: "codex-key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	if _, errFetch := fetcher.FetchQuotaUsage(context.Background(), apiKeyAuth); !errors.Is(errFetch, cliproxyauth.ErrQuotaUsageUnsupported) {
		t.Fatalf("FetchQuotaUsage(api key) error = %v, want ErrQuotaUsageUnsupported", errFetch)
	}
}
