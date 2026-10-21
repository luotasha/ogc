package server

import (
	"github.com/xynogen/ogc/internal/config"
	"github.com/xynogen/ogc/internal/metrics"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPinnedSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings")
	os.WriteFile(path, nil, 0600)
	cfg := &config.Config{SessionBindings: path, Models: map[string]config.ModelConfig{"model-a": {ModelID: "model-a"}}, Accounts: []config.Account{{ID: "a", APIKey: "secret-a", Model: "model-a"}, {ID: "b", APIKey: "secret-b", Model: "model-a"}}}
	makeRouter := func() *sessionRouter {
		s, e := newSessionRouter(cfg, nil, metrics.New(), slog.Default())
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	s := makeRouter()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.choose("session-one"); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if len(s.bindings) != 1 {
		t.Fatal("concurrent assignment duplicated")
	}
	s.choose("session-two")
	if s.bindings[digest("session-one")].Account == s.bindings[digest("session-two")].Account {
		t.Fatal("not distributed")
	}
	cfg.Accounts[0], cfg.Accounts[1] = cfg.Accounts[1], cfg.Accounts[0]
	restarted := makeRouter()
	restarted.choose("session-one")
	resumed := restarted.bindings[digest("session-one")]
	original := s.bindings[digest("session-one")]
	if resumed.Account != original.Account || resumed.Fingerprint != original.Fingerprint {
		t.Fatal("resume reassigned")
	}
	cfg.Accounts = cfg.Accounts[:1]
	missing := makeRouter()
	if _, e := missing.choose("session-one"); e == nil {
		t.Fatal("removed account silently reassigned")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret-a") {
		t.Fatal("key leaked")
	}
	os.WriteFile(path, []byte("broken"), 0600)
	if _, e := newSessionRouter(cfg, nil, metrics.New(), slog.Default()); e == nil {
		t.Fatal("corrupt journal accepted")
	}
	os.Remove(path)
	if _, e := newSessionRouter(cfg, nil, metrics.New(), slog.Default()); e == nil {
		t.Fatal("missing journal accepted")
	}
}
func TestRequestPinningAndFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings")
	os.WriteFile(path, nil, 0600)
	cfg := &config.Config{SessionBindings: path, Models: map[string]config.ModelConfig{"model-a": {ModelID: "model-a"}}, Accounts: []config.Account{{ID: "a", APIKey: "a", Model: "model-a"}, {ID: "b", APIKey: "b", Model: "model-a"}}}
	s, e := newSessionRouter(cfg, nil, metrics.New(), slog.Default())
	if e != nil {
		t.Fatal(e)
	}
	callsA, callsB := 0, 0
	s.handlers["a"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { callsA++; w.WriteHeader(429) })
	s.handlers["b"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { callsB++; w.WriteHeader(200) })
	for _, launcher := range []string{"launch-one", "resume-new-launch"} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"automodel","metadata":{"user_id":"user_x_session_persistent"}}`))
		r.Header.Set("x-opencode-session", launcher)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 429 {
			t.Fatal("failure switched account")
		}
	}
	if callsA != 2 || callsB != 0 {
		t.Fatal("account crossed")
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	r.Header.Set("x-opencode-session", "random-launcher")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("unstable identity accepted")
	}
}

func TestAutoModelPinningAndExplicitConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings")
	os.WriteFile(path, nil, 0600)
	cfg := &config.Config{
		SessionBindings: path,
		Models: map[string]config.ModelConfig{
			"model-a": {ModelID: "model-a"},
			"model-b": {ModelID: "model-b"},
		},
		ModelMapping: map[string]string{"claude-opus": "model-a"},
		Accounts: []config.Account{
			{ID: "a", APIKey: "secret-a", Model: "model-a"},
			{ID: "b", APIKey: "secret-b", Model: "model-b"},
		},
	}
	s, err := newSessionRouter(cfg, nil, metrics.New(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.choose("auto-session"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.selectModel("auto-session", "automodel"); err != nil || got != "model-a" {
		t.Fatalf("automodel selection = %q, %v", got, err)
	}
	s.accounts[0].Model = "model-b"
	if got, err := s.selectModel("auto-session", "automodel"); err != nil || got != "model-a" {
		t.Fatal("account default change altered an existing session")
	}
	if _, err := s.selectModel("auto-session", "model-b"); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatal("different explicit model did not conflict with pinned model")
	}
	if got, err := s.selectModel("auto-session", "claude-opus-4"); err != nil || got != "model-a" {
		t.Fatal("equivalent explicit model mapping was rejected")
	}

	if _, err = s.choose("explicit-session"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.selectModel("explicit-session", "model-a"); err != nil || got != "model-a" {
		t.Fatalf("explicit first model = %q, %v", got, err)
	}
	if got, err := s.selectModel("explicit-session", "automodel"); err != nil || got != "model-a" {
		t.Fatal("automodel did not preserve an explicitly pinned model")
	}

	restarted, err := newSessionRouter(cfg, nil, metrics.New(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.selectModel("auto-session", "automodel"); err != nil || got != "model-a" {
		t.Fatal("model pin did not survive restart")
	}
}
