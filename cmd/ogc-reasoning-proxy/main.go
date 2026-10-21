package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	Reasoning string `json:"reasoning"`
	LastSeen  int64  `json:"last_seen"`
}

type reasoningCache struct {
	mu      sync.RWMutex
	path    string
	entries map[string]cacheEntry
}

func loadCache(path string) *reasoningCache {
	c := &reasoningCache{path: path, entries: map[string]cacheEntry{}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &c.entries)
	}
	c.pruneLocked(time.Now().Add(-30 * 24 * time.Hour).UnixMilli())
	return c
}

func (c *reasoningCache) get(ids []string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, id := range ids {
		if e, ok := c.entries[id]; ok && e.Reasoning != "" {
			return e.Reasoning, true
		}
	}
	return "", false
}

func (c *reasoningCache) put(ids []string, reasoning string) {
	if reasoning == "" || len(ids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixMilli()
	changed := false
	for _, id := range ids {
		if id == "" {
			continue
		}
		if old, ok := c.entries[id]; !ok || old.Reasoning != reasoning {
			c.entries[id] = cacheEntry{Reasoning: reasoning, LastSeen: now}
			changed = true
		}
	}
	if changed {
		c.pruneLocked(time.Now().Add(-30 * 24 * time.Hour).UnixMilli())
		if err := c.persistLocked(); err != nil {
			log.Printf("persist reasoning cache: %v", err)
		}
	}
}

func (c *reasoningCache) pruneLocked(cutoff int64) {
	for id, e := range c.entries {
		if e.LastSeen < cutoff {
			delete(c.entries, id)
		}
	}
}

func (c *reasoningCache) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(c.entries)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

type proxy struct {
	target *url.URL
	client *http.Client
	cache  *reasoningCache
}

const missingReasoningFallback = "Reasoning unavailable because this tool call predates the local compatibility cache."

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	patched, injected := p.patchRequest(body)
	if injected > 0 {
		log.Printf("restored reasoning_content for %d assistant message(s)", injected)
	}

	u := *p.target
	u.Path = strings.TrimRight(p.target.Path, "/") + r.URL.Path
	u.RawQuery = r.URL.RawQuery
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(patched))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Host = p.target.Host
	resp, err := p.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.forwardStream(w, resp.Body)
		return
	}
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		p.captureResponse(b)
		_, _ = w.Write(b)
	}
}

func (p *proxy) patchRequest(body []byte) ([]byte, int) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body, 0
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return body, 0
	}
	injected := 0
	changed := false
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Empty assistant and tool turns are valid in the source protocol, but
		// the OpenCode endpoint requires every chat message to carry content.
		if _, ok := msg["content"]; !ok {
			msg["content"] = ""
			changed = true
		}
		if msg["role"] != "assistant" {
			continue
		}
		if stringValue(msg["reasoning_content"]) != "" {
			continue
		}
		ids := toolCallIDs(msg["tool_calls"])
		if len(ids) == 0 {
			continue
		}
		if reasoning, ok := p.cache.get(ids); ok {
			msg["reasoning_content"] = reasoning
		} else {
			// Sessions created before the proxy was installed, or responses whose
			// streamed tool-call IDs were fragmented by the provider, have no
			// recoverable cache entry. OpenCode Go rejects those turns when the
			// field is absent, but accepts a nonempty compatibility marker.
			msg["reasoning_content"] = missingReasoningFallback
		}
		injected++
		changed = true
	}
	if !changed {
		return body, 0
	}
	b, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return b, injected
}

func (p *proxy) captureResponse(body []byte) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return
	}
	choices, _ := root["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		reasoning := reasoningValue(msg)
		p.cache.put(toolCallIDs(msg["tool_calls"]), reasoning)
	}
}

func (p *proxy) forwardStream(w http.ResponseWriter, body io.Reader) {
	reader := bufio.NewReader(body)
	flusher, _ := w.(http.Flusher)
	var reasoning strings.Builder
	toolCalls := newStreamToolCalls()
	complete := false
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			captureStreamLine(line, &reasoning, toolCalls)
			complete = complete || streamCompleteLine(line)
			// Persist before forwarding the tool-call chunk. Claude Code can run
			// a fast tool and submit its result before the provider emits [DONE],
			// or it can close the stream once it has the call. Saving here gives
			// the follow-up request a happens-before relationship with the cache.
			// Later ID fragments update the entry again under the complete ID.
			p.cache.put(toolCalls.ids(), reasoning.String())
			_, _ = io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	// Keep a final commit for nonstandard streams where the completion marker and
	// last payload arrive together.
	if complete && reasoning.Len() > 0 {
		p.cache.put(toolCalls.ids(), reasoning.String())
	}
}

func streamCompleteLine(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		return true
	}
	var root map[string]any
	if json.Unmarshal([]byte(data), &root) != nil {
		return false
	}
	choices, _ := root["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice["finish_reason"] != nil {
			return true
		}
	}
	return false
}

type streamToolCalls struct {
	idsByIndex map[int]*strings.Builder
}

func newStreamToolCalls() *streamToolCalls {
	return &streamToolCalls{idsByIndex: map[int]*strings.Builder{}}
}

func (s *streamToolCalls) add(index int, fragment string) {
	if fragment == "" {
		return
	}
	b := s.idsByIndex[index]
	if b == nil {
		b = &strings.Builder{}
		s.idsByIndex[index] = b
	}
	b.WriteString(fragment)
}

func (s *streamToolCalls) ids() []string {
	ids := make([]string, 0, len(s.idsByIndex))
	for i := 0; i < len(s.idsByIndex); i++ {
		if b := s.idsByIndex[i]; b != nil && b.Len() > 0 {
			ids = append(ids, b.String())
		}
	}
	return ids
}

func captureStreamLine(line string, reasoning *strings.Builder, toolCalls *streamToolCalls) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	var root map[string]any
	if json.Unmarshal([]byte(data), &root) != nil {
		return
	}
	choices, _ := root["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		reasoning.WriteString(reasoningValue(delta))
		items, _ := delta["tool_calls"].([]any)
		for position, raw := range items {
			item, _ := raw.(map[string]any)
			index := position
			if n, ok := item["index"].(float64); ok && n >= 0 {
				index = int(n)
			}
			toolCalls.add(index, stringValue(item["id"]))
		}
	}
}

func reasoningValue(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	if s := stringValue(msg["reasoning_content"]); s != "" {
		return s
	}
	return stringValue(msg["reasoning"])
}

func toolCallIDs(v any) []string {
	items, _ := v.([]any)
	var ids []string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if id := stringValue(item["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func main() {
	listen := flag.String("listen", "127.0.0.1:3460", "listen address")
	targetRaw := flag.String("target", "https://opencode.ai/zen/go", "upstream URL prefix")
	cachePath := flag.String("cache", "reasoning-cache.json", "cache file")
	flag.Parse()
	target, err := url.Parse(*targetRaw)
	if err != nil {
		log.Fatal(err)
	}
	p := &proxy{
		target: target,
		client: &http.Client{Timeout: 5 * time.Minute},
		cache:  loadCache(*cachePath),
	}
	log.Printf("reasoning compatibility proxy listening on %s -> %s", *listen, target)
	if err := http.ListenAndServe(*listen, p); err != nil {
		log.Fatal(fmt.Errorf("serve: %w", err))
	}
}
