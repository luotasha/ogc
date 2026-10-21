package client

import (
	"context"
	"net/http"
	"testing"
)

func TestSessionHeaders(t *testing.T) {
	for _, tc := range []struct{ h, meta, want string }{
		{"conversation-a", "", "conversation-a"},
		{"", `{"session_id":"conversation-b"}`, "conversation-b"},
		{"", "user_x_account_y_session_conversation-c", "conversation-c"},
	} {
		h := http.Header{}
		if tc.h != "" {
			h.Set("x-opencode-session", tc.h)
		}
		r, _ := http.NewRequestWithContext(WithSession(context.Background(), h, tc.meta), "POST", "http://localhost", nil)
		setSessionHeaders(r)
		if r.Header.Get("x-opencode-session") != tc.want {
			t.Fatal("session not preserved")
		}
		if r.Header.Get("User-Agent") != "ogc/session-fix-1" {
			t.Fatal("user agent missing")
		}
	}
}
