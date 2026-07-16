package maintainer

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/oidc"
)

type Service struct {
	Store    *postgres.Connector
	Redis    *redis.Client
	OIDC     *oidc.Client
	Interval time.Duration
	Batch    int
	Skew     time.Duration
	Enabled  func() bool
	IsLeader func() bool

	mu        sync.Mutex
	started   bool
	stop      chan struct{}
	runSoon   chan struct{}
	last      map[string]any
	forceNext bool
}

func New(store *postgres.Connector, redisClient *redis.Client, oidcClient *oidc.Client) *Service {
	return &Service{
		Store:    store,
		Redis:    redisClient,
		OIDC:     oidcClient,
		Interval: 90 * time.Second,
		Batch:    40,
		Skew:     2 * time.Minute,
		Enabled:  func() bool { return true },
		IsLeader: func() bool { return true },
		stop:     make(chan struct{}),
		runSoon:  make(chan struct{}, 1),
		last:     map[string]any{"ok": true, "started": false},
	}
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

func (s *Service) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stop)
	s.stop = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) RequestRunSoon(force bool) {
	s.mu.Lock()
	s.forceNext = s.forceNext || force
	s.mu.Unlock()
	select {
	case s.runSoon <- struct{}{}:
	default:
	}
}

func (s *Service) Status() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false, "implementation": "go", "started": false}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	for k, v := range s.last {
		out[k] = v
	}
	out["enabled"] = s.Enabled == nil || s.Enabled()
	out["started"] = s.started
	out["implementation"] = "go"
	out["interval_sec"] = s.Interval.Seconds()
	out["batch"] = s.Batch
	out["is_leader"] = s.IsLeader == nil || s.IsLeader()
	return out
}

func (s *Service) loop() {
	// short startup delay like Python
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun(false)
			timer.Reset(s.Interval)
		case <-s.runSoon:
			force := false
			s.mu.Lock()
			force = s.forceNext
			s.forceNext = false
			s.mu.Unlock()
			s.maybeRun(force)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.Interval)
		}
	}
}

func (s *Service) maybeRun(force bool) {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, force)
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
}

// RunOnce performs one normalize-ish + refresh cycle against PostgreSQL.
func (s *Service) RunOnce(ctx context.Context, force bool) map[string]any {
	result := map[string]any{
		"ok":             true,
		"force":          force,
		"implementation": "go",
		"at":             time.Now().Unix(),
	}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	// maintenance lock best-effort
	if s.Redis != nil && s.Redis.Enabled() {
		ok, release, err := s.Redis.AcquireMaintenanceLock(ctx, "token_maintainer", 180*time.Second, true)
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			result["error"] = "maintenance slot busy — deferred"
			return result
		}
	}
	if n, err := s.Store.ExpireDueCooldowns(ctx, 200); err == nil {
		result["expired_cooldowns"] = n
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 40
	}
	rows, err := s.Store.ListRefreshableAccounts(ctx, batch*2)
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	now := time.Now()
	skew := s.Skew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	candidates := make([]postgres.AccountRefreshRow, 0, batch)
	for _, row := range rows {
		rt := stringFrom(row.Payload, "refresh_token")
		if rt == "" {
			continue
		}
		if truthy(row.Payload["refresh_invalid"]) {
			continue
		}
		if !force {
			exp := accounts.ParseExpiresAt(row.Payload["expires_at"], stringFrom(row.Payload, "key"))
			if exp != nil && float64(now.Unix())+skew.Seconds() < *exp {
				continue
			}
		}
		candidates = append(candidates, row)
		if len(candidates) >= batch {
			break
		}
	}
	refreshed, failed, skipped, deleted := 0, 0, 0, 0
	failedSample := []map[string]any{}
	oidcClient := s.OIDC
	if oidcClient == nil {
		oidcClient = &oidc.Client{}
	}
	for _, row := range candidates {
		tokenData, err := oidcClient.RefreshAccessToken(ctx, row.Payload)
		if err != nil {
			failed++
			permanent := false
			errText := err.Error()
			var re *oidc.RefreshError
			if asRefresh(err, &re) {
				permanent = re.Permanent
				errText = re.Error()
			}
			if permanent {
				_ = s.Store.MarkRefreshInvalid(ctx, row.ID, errText)
				// hard-delete accounts without SSO when RT permanently dead
				if accounts.GetSSOValue(row.Payload) == "" {
					if ok, _ := s.Store.DeleteAccount(ctx, row.ID); ok {
						deleted++
					}
				}
			}
			if len(failedSample) < 5 {
				failedSample = append(failedSample, map[string]any{"id": row.ID, "error": errText, "permanent": permanent})
			}
			continue
		}
		newID, entry, err := oidc.EntryFromTokenResponse(tokenData, row.Payload)
		if err != nil {
			failed++
			continue
		}
		if newID == "" {
			newID = row.ID
		}
		if newID != row.ID {
			// move row if storage id changed
			_ = s.Store.UpsertAccount(ctx, newID, entry)
			if newID != row.ID {
				_, _ = s.Store.DeleteAccount(ctx, row.ID)
			}
		} else {
			_ = s.Store.UpsertAccount(ctx, row.ID, entry)
		}
		// clear cooldown after successful renew
		_, _ = s.Store.ClearAccountCooldown(ctx, newID)
		refreshed++
	}
	result["refresh"] = map[string]any{
		"attempted":     len(candidates),
		"refreshed":     refreshed,
		"failed":        failed,
		"skipped":       skipped,
		"deleted":       deleted,
		"failed_sample": failedSample,
		"workers":       1,
		"batch":         batch,
	}
	result["accounts_total"] = len(rows)
	slog.Info("token maintainer cycle", "attempted", len(candidates), "refreshed", refreshed, "failed", failed, "deleted", deleted)
	return result
}

func asRefresh(err error, target **oidc.RefreshError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*oidc.RefreshError); ok {
		*target = e
		return true
	}
	return false
}

func stringFrom(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}
