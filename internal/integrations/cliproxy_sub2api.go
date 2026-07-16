package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

type Store interface {
	PublicSettings(ctx context.Context) (map[string]any, error)
	SetSetting(ctx context.Context, key string, value any) error
	GetSetting(ctx context.Context, key string) (any, error)
	ExportAuthMap(ctx context.Context, accountIDs []string, includeSecrets bool) (map[string]any, error)
}

func PublicConfig(ctx context.Context, store Store, key string) map[string]any {
	out := map[string]any{"enabled": false, "base_url": ""}
	if store == nil {
		return out
	}
	settings, err := store.PublicSettings(ctx)
	if err != nil {
		return out
	}
	raw, _ := settings[key].(map[string]any)
	if raw == nil {
		return out
	}
	// PublicSettings already redacts secrets; pass through.
	return raw
}

func SaveConfig(ctx context.Context, store Store, key string, patch map[string]any) (map[string]any, error) {
	if store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	cur := map[string]any{}
	if raw, err := store.GetSetting(ctx, key); err == nil {
		if m, ok := raw.(map[string]any); ok {
			cur = m
		}
	}
	// merge
	for k, v := range patch {
		if k == "management_key" || k == "password" {
			if strings.TrimSpace(fmt.Sprint(v)) == "" {
				continue // keep previous secret
			}
		}
		cur[k] = v
	}
	if err := store.SetSetting(ctx, key, cur); err != nil {
		return nil, err
	}
	return PublicConfig(ctx, store, key), nil
}

func ExportCLIProxyBundle(ctx context.Context, store Store, ids []string) (map[string]any, error) {
	authMap, err := store.ExportAuthMap(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	auth, _ := authMap["auth"].(map[string]any)
	accountsOut := []map[string]any{}
	skipped := 0
	for aid, raw := range auth {
		entry, _ := raw.(map[string]any)
		rec := buildCLIProxyRecord(entry, aid)
		if rec == nil {
			skipped++
			continue
		}
		accountsOut = append(accountsOut, rec)
	}
	return map[string]any{
		"type":        "cliproxyapi-auth-bundle",
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    accountsOut,
		"count":       len(accountsOut),
		"skipped":     skipped,
		"ok":          true,
	}, nil
}

func ExportSub2APIFormat(ctx context.Context, store Store, ids []string) (map[string]any, error) {
	authMap, err := store.ExportAuthMap(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	auth, _ := authMap["auth"].(map[string]any)
	items := []map[string]any{}
	for aid, raw := range auth {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		token := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"), stringField(entry, "token"))
		if token == "" {
			continue
		}
		items = append(items, map[string]any{
			"id":            aid,
			"email":         stringField(entry, "email"),
			"access_token":  token,
			"refresh_token": stringField(entry, "refresh_token"),
			"expires_at":    entry["expires_at"],
			"sso":           accounts.GetSSOValue(entry),
		})
	}
	return map[string]any{
		"ok":          true,
		"format":      "sub2api-oauth-export",
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    items,
		"count":       len(items),
	}, nil
}

func buildCLIProxyRecord(entry map[string]any, aid string) map[string]any {
	if entry == nil {
		return nil
	}
	access := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"), stringField(entry, "token"))
	if access == "" {
		return nil
	}
	claims := accounts.DecodeJWTClaims(access)
	email := firstNonEmpty(stringField(entry, "email"), stringField(claims, "email"))
	sub := firstNonEmpty(stringField(entry, "user_id"), stringField(entry, "principal_id"), stringField(entry, "sub"), stringField(claims, "principal_id"), stringField(claims, "sub"))
	expiredISO := ""
	if exp := accounts.ParseExpiresAt(entry["expires_at"], access); exp != nil {
		expiredISO = time.Unix(int64(*exp), 0).UTC().Format(time.RFC3339)
	}
	headers, _ := entry["cliproxyapi_headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{
			"X-XAI-Token-Auth":         "xai-grok-cli",
			"x-grok-client-version":    "0.2.93",
			"x-grok-client-identifier": "grok-shell",
		}
	}
	baseURL := firstNonEmpty(stringField(entry, "cliproxyapi_base_url"), "https://cli-chat-proxy.grok.com/v1")
	rec := map[string]any{
		"type":          firstNonEmpty(stringField(entry, "cliproxyapi_type"), "xai"),
		"auth_kind":     firstNonEmpty(stringField(entry, "cliproxyapi_auth_kind"), "oauth"),
		"email":         email,
		"sub":           sub,
		"access_token":  access,
		"refresh_token": stringField(entry, "refresh_token"),
		"id_token":      stringField(entry, "id_token"),
		"token_type":    "Bearer",
		"expired":       expiredISO,
		"last_refresh":  time.Now().UTC().Format(time.RFC3339),
		"base_url":      baseURL,
		"disabled":      false,
		"headers":       headers,
	}
	if aid != "" {
		rec["local_account_id"] = aid
	}
	if sub != "" {
		rec["account_id"] = sub
	}
	return rec
}

// TestCLIProxy does a lightweight management auth-files list.
func TestCLIProxy(ctx context.Context, cfg map[string]any) map[string]any {
	base := strings.TrimRight(stringField(cfg, "base_url"), "/")
	key := stringField(cfg, "management_key")
	if base == "" || key == "" {
		return map[string]any{"ok": false, "error": "base_url and management_key required"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v0/management/auth-files", nil)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Management-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return map[string]any{"ok": false, "status_code": resp.StatusCode, "error": string(body)}
	}
	return map[string]any{"ok": true, "status_code": resp.StatusCode, "body_preview": string(body[:min(200, len(body))])}
}

// PushCLIProxy uploads selected/all accounts as auth files.
func PushCLIProxy(ctx context.Context, store Store, ids []string, concurrency int) (map[string]any, error) {
	raw := map[string]any{}
	if v, err := store.GetSetting(ctx, "cliproxyapi_config"); err == nil {
		if m, ok := v.(map[string]any); ok {
			raw = m
		}
	}
	base := strings.TrimRight(stringField(raw, "base_url"), "/")
	key := stringField(raw, "management_key")
	if base == "" {
		return map[string]any{"ok": false, "error": "CLIProxyAPI base_url missing"}, nil
	}
	if key == "" {
		return map[string]any{"ok": false, "error": "CLIProxyAPI management_key missing (ensure secret is stored)"}, nil
	}
	bundle, err := ExportCLIProxyBundle(ctx, store, ids)
	if err != nil {
		return nil, err
	}
	list, _ := bundle["accounts"].([]map[string]any)
	// type assert from []any if needed
	if list == nil {
		if arr, ok := bundle["accounts"].([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					list = append(list, m)
				}
			}
		}
	}
	success, failed := 0, 0
	errors := []map[string]any{}
	client := &http.Client{Timeout: 45 * time.Second}
	for _, rec := range list {
		name := firstNonEmpty(stringField(rec, "email"), stringField(rec, "sub"), "account")
		name = sanitizeName(name) + ".json"
		payload, _ := json.Marshal(rec)
		url := base + "/v0/management/auth-files?name=" + name
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			failed++
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Management-Key", key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			failed++
			errors = append(errors, map[string]any{"name": name, "error": err.Error()})
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			failed++
			errors = append(errors, map[string]any{"name": name, "status": resp.StatusCode, "error": string(body)})
			continue
		}
		success++
	}
	return map[string]any{
		"ok":      failed == 0,
		"success": success,
		"failed":  failed,
		"total":   len(list),
		"errors":  errors,
		"message": fmt.Sprintf("CLIProxyAPI push: success=%d failed=%d", success, failed),
	}, nil
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "@", "_at_")
	out := strings.Builder{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "account"
	}
	return out.String()
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
