package modelhealth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

type Service struct {
	Store       *postgres.Connector
	Redis       *redis.Client
	Upstream    string
	Models      []string
	Interval    time.Duration
	Batch       int
	AutoDisable bool
	Enabled     func() bool
	IsLeader    func() bool

	mu      sync.Mutex
	started bool
	stop    chan struct{}
	runSoon chan struct{}
	last    map[string]any
}

func New(store *postgres.Connector, redisClient *redis.Client, upstream string, models []string) *Service {
	if len(models) == 0 {
		models = []string{"grok-4.5"}
	}
	return &Service{
		Store:       store,
		Redis:       redisClient,
		Upstream:    upstream,
		Models:      models,
		Interval:    15 * time.Minute,
		Batch:       30,
		AutoDisable: true,
		Enabled:     func() bool { return true },
		IsLeader:    func() bool { return true },
		stop:        make(chan struct{}),
		runSoon:     make(chan struct{}, 1),
		last:        map[string]any{"ok": true, "started": false},
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

func (s *Service) RequestRunSoon() {
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
	out["models"] = append([]string{}, s.Models...)
	out["is_leader"] = s.IsLeader == nil || s.IsLeader()
	return out
}

func (s *Service) loop() {
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun()
			timer.Reset(s.Interval)
		case <-s.runSoon:
			s.maybeRun()
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

func (s *Service) maybeRun() {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, "background")
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
}

func (s *Service) RunOnce(ctx context.Context, source string) map[string]any {
	result := map[string]any{"ok": true, "source": source, "implementation": "go", "at": time.Now().Unix()}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	if s.Redis != nil && s.Redis.Enabled() {
		ok, release, err := s.Redis.AcquireMaintenanceLock(ctx, "model_health", 180*time.Second, true)
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			return result
		}
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 30
	}
	auths, err := s.Store.ListAccountAuths(ctx, batch, true)
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	model := s.Models[0]
	available, failed := 0, 0
	samples := []map[string]any{}
	for _, auth := range auths {
		probe := s.ProbeAccount(ctx, auth, model, source)
		if probe["available"] == true {
			available++
		} else {
			failed++
			if len(samples) < 5 {
				samples = append(samples, probe)
			}
		}
	}
	result["probed"] = len(auths)
	result["available"] = available
	result["failed"] = failed
	result["failed_sample"] = samples
	result["model"] = model
	slog.Info("model health cycle", "probed", len(auths), "available", available, "failed", failed, "model", model)
	return result
}

func (s *Service) ProbeAccount(ctx context.Context, auth postgres.AccountAuth, model, source string) map[string]any {
	if model == "" && len(s.Models) > 0 {
		model = s.Models[0]
	}
	started := time.Now()
	base := map[string]any{
		"ok":         false,
		"available":  false,
		"account_id": auth.ID,
		"email":      auth.Email,
		"model":      model,
		"probed_at":  started.Unix(),
		"source":     source,
	}
	client := &grok.Client{BaseURL: s.Upstream, HTTP: &http.Client{Timeout: 20 * time.Second}}
	body := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 8,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
	}
	resp, err := client.Open(ctx, grok.Account{ID: auth.ID, Token: auth.Token}, model, body)
	if err != nil {
		status := 0
		errText := err.Error()
		var ue *grok.UpstreamError
		if asUpstream(err, &ue) {
			status = ue.Status
			errText = ue.Body
			if len(errText) > 400 {
				errText = errText[:400]
			}
		}
		base["status_code"] = status
		base["error"] = errText
		base["latency_ms"] = time.Since(started).Milliseconds()
		if s.AutoDisable && (status == 401 || status == 403) {
			_, _ = s.Store.SetAccountEnabled(ctx, auth.ID, false)
			base["auto_disabled"] = true
		} else if s.AutoDisable && (status == 429 || status >= 500) {
			sec := 300.0
			_, _ = s.Store.KickFromPool(ctx, auth.ID, errText, &sec)
		}
		_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		return base
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close()
	base["ok"] = true
	base["available"] = true
	base["status_code"] = resp.StatusCode
	base["latency_ms"] = time.Since(started).Milliseconds()
	// recovery: clear cooldown on success
	_, _ = s.Store.ClearAccountCooldown(ctx, auth.ID)
	_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
	return base
}

func (s *Service) ProbeIDs(ctx context.Context, ids []string, model string, autoDisable bool, source string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		auth, err := s.Store.GetAccountAuth(ctx, id)
		if err != nil {
			out = append(out, map[string]any{"ok": false, "account_id": id, "error": err.Error()})
			continue
		}
		old := s.AutoDisable
		s.AutoDisable = autoDisable
		probe := s.ProbeAccount(ctx, *auth, model, source)
		s.AutoDisable = old
		out = append(out, map[string]any{
			"ok":         probe["available"] == true,
			"account_id": auth.ID,
			"email":      auth.Email,
			"result":     probe,
		})
	}
	return out
}

func asUpstream(err error, target **grok.UpstreamError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*grok.UpstreamError); ok {
		*target = e
		return true
	}
	// errors.As alternative without importing errors for wrapped cases
	if strings.Contains(err.Error(), "upstream status") {
		return false
	}
	return false
}
