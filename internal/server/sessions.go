package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xynogen/ogc/internal/client"
	"github.com/xynogen/ogc/internal/config"
	"github.com/xynogen/ogc/internal/handlers"
	"github.com/xynogen/ogc/internal/metrics"
	"github.com/xynogen/ogc/internal/router"
	"github.com/xynogen/ogc/internal/token"
)

type binding struct {
	Session     string `json:"session"`
	SessionID   string `json:"session_id,omitempty"`
	Account     string `json:"account"`
	Fingerprint string `json:"fingerprint"`
	Model       string `json:"model,omitempty"`
	LastSeen    int64  `json:"last_seen,omitempty"`
}
type sessionRouter struct {
	mu       sync.Mutex
	path     string
	bindings map[string]binding
	accounts []config.Account
	breakers map[string]*router.FallbackHandler
	handlers map[string]http.Handler
	next     int
	poisoned bool
	cfg      *config.Config
	counter  *token.Counter
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func newSessionRouter(cfg *config.Config, counter *token.Counter, m *metrics.Metrics, logger *slog.Logger) (*sessionRouter, error) {
	s := &sessionRouter{path: cfg.SessionBindings, bindings: map[string]binding{}, accounts: cfg.Accounts, handlers: map[string]http.Handler{}, breakers: map[string]*router.FallbackHandler{}, cfg: cfg, counter: counter, metrics: m, logger: logger}
	// Provision this file explicitly. Missing/deleted state must never silently reassign sessions.
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read session bindings: %w (initialize bindings before enabling accounts)", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var b binding
		err := dec.Decode(&b)
		if err == io.EOF {
			break
		}
		if err != nil || b.Session == "" || b.Account == "" || b.Fingerprint == "" {
			return nil, fmt.Errorf("invalid session binding journal")
		}
		// Reassignments are appended to the journal; the last durable record wins.
		s.bindings[b.Session] = b
	}
	s.next = len(s.bindings)
	for _, a := range cfg.Accounts {
		s.addRuntimeAccount(a)
	}
	return s, nil
}

func (s *sessionRouter) addRuntimeAccount(a config.Account) {
	c := *s.cfg
	c.APIKey = a.APIKey
	fb := router.NewFallbackHandler(s.logger, 3, 30*time.Second)
	s.breakers[a.ID] = fb
	h := handlers.NewMessagesHandler(&c, client.NewClient(c.Upstream, a.APIKey), router.NewModelRouter(&c), fb, s.counter, s.metrics)
	s.handlers[a.ID] = http.HandlerFunc(h.HandleMessages)
}
func (s *sessionRouter) choose(id string) (http.Handler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, fmt.Errorf("binding storage failed; restart after repairing journal")
	}
	hash := digest(id)
	if b, ok := s.bindings[hash]; ok {
		for _, a := range s.accounts {
			if a.ID != b.Account {
				continue
			}
			if a.Disabled {
				return nil, fmt.Errorf("session's pinned account is disabled")
			}
			if digest(a.APIKey) == b.Fingerprint {
				now := time.Now().UnixMilli()
				previous := b.LastSeen
				b.LastSeen = now
				if b.SessionID == "" {
					b.SessionID = id
				}
				s.bindings[hash] = b
				// Keep the UI current in memory, but limit durable journal writes.
				if previous == 0 || now-previous >= int64(time.Minute/time.Millisecond) {
					if err := s.appendBinding(b); err != nil {
						s.logger.Warn("failed to persist session activity", "error", err)
					}
				}
				return s.handlers[a.ID], nil
			}
		}
		return nil, fmt.Errorf("session's pinned account/key unavailable; refusing reassignment")
	}
	enabled := make([]config.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		if !a.Disabled {
			enabled = append(enabled, a)
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no enabled accounts")
	}
	a := enabled[s.next%len(enabled)]
	b := binding{Session: hash, SessionID: id, Account: a.ID, Fingerprint: digest(a.APIKey), LastSeen: time.Now().UnixMilli()}
	data, _ := json.Marshal(b)
	data = append(data, '\n')
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open binding storage: %w", err)
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("persist binding: %w", err)
	}
	s.bindings[hash] = b
	s.next++
	return s.handlers[a.ID], nil
}

func (s *sessionRouter) selectModel(id, requested string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[digest(id)]
	if !ok {
		return "", fmt.Errorf("session binding not found")
	}
	actual := ""
	if requested == "automodel" {
		if b.Model != "" {
			return b.Model, nil
		}
		for _, a := range s.accounts {
			if a.ID == b.Account {
				actual = a.Model
				break
			}
		}
	} else {
		actual = s.resolveModel(requested)
		if actual == "" {
			return "", fmt.Errorf("requested model %q is not configured", requested)
		}
	}
	if actual == "" {
		return "", fmt.Errorf("no default model configured for the session's account")
	}
	if b.Model != "" {
		if b.Model != actual {
			return "", fmt.Errorf("session is pinned to model %q; requested %q resolves to %q", b.Model, requested, actual)
		}
		return b.Model, nil
	}
	b.Model = actual
	if err := s.appendBinding(b); err != nil {
		return "", fmt.Errorf("persist session model: %w", err)
	}
	s.bindings[b.Session] = b
	return actual, nil
}

func (s *sessionRouter) resolveModel(requested string) string {
	if target, ok := s.cfg.ModelMapping[requested]; ok {
		if _, exists := s.cfg.Models[target]; exists {
			return target
		}
	}
	for prefix, target := range s.cfg.ModelMapping {
		if strings.HasPrefix(requested, prefix) {
			if _, exists := s.cfg.Models[target]; exists {
				return target
			}
		}
	}
	if _, ok := s.cfg.Models[requested]; ok {
		return requested
	}
	return ""
}
func (s *sessionRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", 413)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	// Multi-account mode requires Claude's stable ID; random launcher headers are not safe for resume.
	headers := r.Header.Clone()
	headers.Del("x-opencode-session")
	id := client.SessionID(headers, req.Metadata.UserID)
	if id == "" {
		http.Error(w, "stable session ID required in metadata.user_id or x-claude-code-session-id", 400)
		return
	}
	h, err := s.choose(id)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	actualModel, err := s.selectModel(id, req.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	raw["model"], _ = json.Marshal(actualModel)
	body, err = json.Marshal(raw)
	if err != nil {
		http.Error(w, "failed to rewrite model", http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.ServeHTTP(w, r)
}

func (s *sessionRouter) Health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	count, broken := len(s.bindings), s.poisoned
	s.mu.Unlock()
	states := map[string]interface{}{}
	for id, fb := range s.breakers {
		states[id] = fb.GetCircuitStates()
	}
	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	if broken {
		status = "binding_storage_failed"
		w.WriteHeader(503)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "service": "ogc", "mode": "session-pinned", "bound_sessions": count, "accounts": len(s.accounts), "circuit_breakers": states})
}
