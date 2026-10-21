package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xynogen/ogc/internal/config"
	"github.com/xynogen/ogc/internal/metrics"
)

func adminTestRouter(t *testing.T) *sessionRouter {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	bindingsPath := filepath.Join(dir, "bindings.jsonl")
	if err := os.WriteFile(configPath, []byte(`{"host":"127.0.0.1","accounts":[{"id":"a","name":"主账号","api_key":"secret-a"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bindingsPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigPath: configPath, SessionBindings: bindingsPath, Models: map[string]config.ModelConfig{"model-a": {ModelID: "model-a"}, "model-b": {ModelID: "model-b"}}, Accounts: []config.Account{{ID: "a", Name: "主账号", APIKey: "secret-a", Model: "model-a"}}}
	s, err := newSessionRouter(cfg, nil, metrics.New(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adminRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = "127.0.0.1:3456"
	if method != http.MethodGet {
		r.Header.Set("Origin", "http://127.0.0.1:3456")
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestAdminAccountAndSessionLifecycle(t *testing.T) {
	s := adminTestRouter(t)

	w := httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPost, "/admin/api/accounts", `{"APIKey":"secret-b"}`))
	if w.Code != http.StatusCreated || len(s.accounts) != 2 {
		t.Fatalf("add account: status=%d body=%s", w.Code, w.Body.String())
	}

	if _, err := s.choose("session-visible"); err != nil {
		t.Fatal(err)
	}
	if model, err := s.selectModel("session-visible", "automodel"); err != nil || model != "model-a" {
		t.Fatal("failed to pin initial account model")
	}
	hash := digest("session-visible")
	if s.bindings[hash].LastSeen == 0 {
		t.Fatal("session activity time was not recorded")
	}
	target := s.accounts[1].ID
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/sessions/"+hash, `{"account_id":"`+target+`"}`))
	if w.Code != http.StatusOK || s.bindings[hash].Account != target {
		t.Fatalf("move session: status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/accounts/"+target, `{"Model":"model-b"}`))
	if w.Code != http.StatusOK || s.accounts[1].Model != "model-b" {
		t.Fatalf("change default model: status=%d body=%s", w.Code, w.Body.String())
	}
	if s.bindings[hash].Model != "model-a" {
		t.Fatal("changing account default model altered an existing session")
	}
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/sessions/"+hash, `{"model":"model-b"}`))
	if w.Code != http.StatusOK || s.bindings[hash].Model != "model-b" || s.bindings[hash].Account != target {
		t.Fatalf("change session model: status=%d body=%s binding=%+v", w.Code, w.Body.String(), s.bindings[hash])
	}
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/sessions/"+hash, `{"model":"missing"}`))
	if w.Code != http.StatusBadRequest || s.bindings[hash].Model != "model-b" {
		t.Fatalf("invalid session model changed binding: status=%d binding=%+v", w.Code, s.bindings[hash])
	}

	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/accounts/"+target, `{"Enabled":false}`))
	if w.Code != http.StatusOK {
		t.Fatalf("disable account: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := s.choose("session-visible"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("session used a disabled account or crossed to another key")
	}
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/accounts/a", `{"Enabled":false}`))
	if w.Code != http.StatusConflict {
		t.Fatal("disabled the last enabled account")
	}
	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodPatch, "/admin/api/accounts/"+target, `{"Enabled":true}`))
	if w.Code != http.StatusOK {
		t.Fatal("failed to re-enable account")
	}
	if _, err := s.choose("session-visible"); err != nil {
		t.Fatal("session did not recover after re-enabling account")
	}

	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodDelete, "/admin/api/accounts/"+target, ""))
	if w.Code != http.StatusConflict {
		t.Fatal("deleted account that still owns a session")
	}

	w = httptest.NewRecorder()
	s.AdminAPI(w, adminRequest(http.MethodGet, "/admin/api/state", ""))
	if strings.Contains(w.Body.String(), "secret-a") || strings.Contains(w.Body.String(), "secret-b") {
		t.Fatal("admin state leaked a full key")
	}
	var state map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "账号 2") || !json.Valid(data) {
		t.Fatal("account changes were not persisted as valid JSON")
	}

	restarted, err := newSessionRouter(s.cfg, nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.bindings[hash].Account != target || restarted.bindings[hash].Model != "model-b" {
		t.Fatal("session reassignment or model change was not durable")
	}
	if restarted.bindings[hash].LastSeen == 0 {
		t.Fatal("session activity time was not durable")
	}
}

func TestAdminRejectsRemoteAndCrossOrigin(t *testing.T) {
	s := adminTestRouter(t)
	w := httptest.NewRecorder()
	s.Admin(w, adminRequest(http.MethodGet, "/admin/", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `type=\"checkbox\"`) || !strings.Contains(w.Body.String(), "changeSessionModel") || strings.Contains(w.Body.String(), "<th>状态</th>") {
		t.Fatal("admin page does not use the compact account controls")
	}

	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	w = httptest.NewRecorder()
	s.Admin(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("remote admin access accepted")
	}

	r = adminRequest(http.MethodPost, "/admin/api/accounts", `{}`)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	s.AdminAPI(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-origin mutation accepted")
	}
}

func TestSessionPreviewUsesRecentConversationText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := strings.Join([]string{
		`{"type":"user","message":{"content":"第一条问题"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"第一条回答"},{"type":"tool_use","name":"x"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"large output"},{"type":"text","text":"第二条问题"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"支线内容"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"第二条回答"}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	got := sessionPreview(path)
	want := "你：第一条问题\nClaude：第一条回答\n你：第二条问题\nClaude：第二条回答"
	if got != want {
		t.Fatalf("preview = %q, want %q", got, want)
	}
}
