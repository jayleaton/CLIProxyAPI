package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewCodexAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now

	planType := strings.TrimSpace(td.PlanType)
	if planType == "" && td.IDToken != "" {
		if claims, errParse := codexauth.ParseJWTToken(td.IDToken); errParse == nil && claims != nil {
			planType = claims.GetPlanType()
		}
	}
	if planType == "" {
		planType = codexauth.DefaultPlanType
	}
	auth.Metadata["plan_type"] = planType
	clonedAttributes := make(map[string]string, len(auth.Attributes)+1)
	for k, v := range auth.Attributes {
		clonedAttributes[k] = v
	}
	clonedAttributes["plan_type"] = planType
	auth.Attributes = clonedAttributes
	if storage, ok := auth.Storage.(*codexauth.CodexTokenStorage); ok && storage != nil {
		clonedStorage := *storage
		svc.UpdateTokenStorage(&clonedStorage, td)
		clonedStorage.PlanType = planType
		auth.Storage = &clonedStorage
	}
	return auth, nil
}

const (
	// codexUsageURL is the ChatGPT backend usage endpoint the native client polls for its
	// rate limit display. It is a read and does not consume model quota.
	codexUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageTimeout = 10 * time.Second
	codexUsageMaxBody = 1 << 20
)

// FetchQuotaUsage reads the ChatGPT rate limit windows used to rank credentials by quota
// reset. API-key credentials and custom backends have no usage endpoint.
func (e *CodexExecutor) FetchQuotaUsage(ctx context.Context, auth *cliproxyauth.Auth) (http.Header, error) {
	if e == nil || auth == nil || codexAuthUsesAPIKey(auth) {
		return nil, cliproxyauth.ErrQuotaUsageUnsupported
	}
	token, baseURL := codexCreds(auth)
	if strings.TrimSpace(token) == "" || (baseURL != "" && !strings.HasPrefix(baseURL, "https://chatgpt.com/")) {
		return nil, cliproxyauth.ErrQuotaUsageUnsupported
	}
	usageCtx, cancelUsage := context.WithTimeout(ctx, codexUsageTimeout)
	defer cancelUsage()
	req, errRequest := http.NewRequestWithContext(usageCtx, http.MethodGet, codexUsageURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create codex usage request: %w", errRequest)
	}
	userAgent, _ := codexHeaderDefaults(e.cfg, auth)
	if userAgent == "" {
		userAgent = codexUserAgent
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", codexOriginator)
	if accountID, ok := auth.Metadata["account_id"].(string); ok && accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	resp, errDo := helps.NewUtlsHTTPClient(usageCtx, e.cfg, auth, 0).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch codex usage: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close usage response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, codexUsageMaxBody))
	if errRead != nil {
		return nil, fmt.Errorf("read codex usage response: %w", errRead)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch codex usage failed with status %d", resp.StatusCode)
	}
	var usage helps.CodexUsage
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return nil, fmt.Errorf("parse codex usage response: %w", errUnmarshal)
	}
	return helps.CodexUsageQuotaHeaders(&usage), nil
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if e == nil {
		return nil
	}
	return resolveCodexKeyConfig(e.cfg, auth)
}

func resolveCodexKeyConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
		if index, errIndex := strconv.Atoi(strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeConfigIndex])); errIndex == nil && index >= 0 && index < len(cfg.CodexKey) {
			entry := &cfg.CodexKey[index]
			cfgKey := strings.TrimSpace(entry.APIKey)
			cfgBase := strings.TrimSpace(entry.BaseURL)
			if (attrKey == "" || strings.EqualFold(cfgKey, attrKey)) && (attrBase == "" || strings.EqualFold(cfgBase, attrBase)) {
				return entry
			}
		}
	}
	for i := range cfg.CodexKey {
		entry := &cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range cfg.CodexKey {
			entry := &cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (e *CodexExecutor) resolveCodexModelIsCompat(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, baseModel string) bool {
	if modelInfo, ok := cliproxyauth.ResolvedModelInfo(req); ok && modelInfo != nil {
		return modelInfo.IsCompat
	}
	entry := e.resolveCodexConfig(auth)
	if entry != nil && len(entry.Models) > 0 {
		requested := strings.TrimSpace(req.Model)
		target := strings.TrimSpace(baseModel)
		for i := range entry.Models {
			name := strings.TrimSpace(entry.Models[i].Name)
			alias := strings.TrimSpace(entry.Models[i].Alias)
			if (target != "" && (strings.EqualFold(name, target) || strings.EqualFold(alias, target))) ||
				(requested != "" && (strings.EqualFold(name, requested) || strings.EqualFold(alias, requested))) {
				return entry.Models[i].IsCompat
			}
		}
		return false
	}
	if cliproxyauth.CodexAPIKeyModelIsCompat(e.cfg, auth, baseModel) || cliproxyauth.CodexAPIKeyModelIsCompat(e.cfg, auth, req.Model) {
		return true
	}
	return false
}
