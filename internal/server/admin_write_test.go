package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestAdminWriteRoutesGated(t *testing.T) {
	for _, path := range []string{"/admin/api/login", "/admin/api/setup", "/admin/api/keys", "/admin/api/accounts/x/kick"} {
		rec := httptest.NewRecorder()
		NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s disabled = %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminLoginEnvBootstrapWithoutStoreHash(t *testing.T) {
	// Without store, login should still fail closed on store unavailable.
	rec := httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		AdminReadEnabled:  true,
		AdminWriteEnabled: true,
		Config:            config.Config{LegacyAdminPassword: "bootstrap-pass"},
	}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"bootstrap-pass"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without store = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSessionUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: false}}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/session", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session unauthorized = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != false {
		t.Fatalf("body %#v", body)
	}
}
