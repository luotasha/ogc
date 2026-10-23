package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type sessionKey struct{}

func WithSession(ctx context.Context, h http.Header, userID string) context.Context {
	id := SessionID(h, userID)

	return context.WithValue(ctx, sessionKey{}, id)
}
func setSessionHeaders(r *http.Request) {
	if id, ok := r.Context().Value(sessionKey{}).(string); ok && id != "" {
		r.Header.Set("x-opencode-session", id)
	}
	r.Header.Set("User-Agent", "ogc/session-fix-1")
}

// resetUpstreamSession keeps OGC's local binding intact while replacing an
// upstream session that has started returning a transient 401. The request
// body contains the full conversation, so upstream continuity is preserved.
func resetUpstreamSession(r *http.Request) {
	id := r.Header.Get("x-opencode-session")
	if id == "" {
		id = "ogc"
	}
	r.Header.Set("x-opencode-session", id+"-reset-"+time.Now().UTC().Format("20060102T150405.000000000"))
}

// SessionID prefers Claude's persistent identity over launcher-generated headers.
func SessionID(h http.Header, userID string) string {
	var meta struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(userID), &meta) == nil && meta.SessionID != "" {
		return meta.SessionID
	}
	if i := strings.LastIndex(userID, "_session_"); i >= 0 && userID[i+9:] != "" {
		return userID[i+9:]
	}
	if id := h.Get("x-claude-code-session-id"); id != "" {
		return id
	}
	return h.Get("x-opencode-session")
}
