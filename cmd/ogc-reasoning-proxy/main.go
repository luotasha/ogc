package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultCacheTTL           = 30 * 24 * time.Hour
	defaultCacheMaxEntries    = 30000
	defaultCacheFlushInterval = 30 * time.Second
	cacheTouchInterval        = time.Hour
	defaultMemoryLimitMiB     = 512
)

type cacheEntry struct {
	Reasoning string `json:"reasoning"`
	LastSeen  int64  `json:"last_seen"`
}

type reasoningCache struct {
	mu            sync.RWMutex
	path          string
	entries       map[string]cacheEntry
	ttl           time.Duration
	maxEntries    int
	flushInterval time.Duration
	dirty         bool
	version       uint64
	stop          chan struct{}
	done          chan struct{}
}

func loadCache(path string, ttl time.Duration, maxEntries int, flushInterval time.Duration) *reasoningCache {
	c := &reasoningCache{
		path:          path,
		entries:       map[string]cacheEntry{},
		ttl:           ttl,
		maxEntries:    maxEntries,
		flushInterval: flushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	if f, err := os.Open(path); err == nil {
		if err := json.NewDecoder(bufio.NewReaderSize(f, 256*1024)).Decode(&c.entries); err != nil {
			log.Printf("load reasoning cache: %v", err)
		}
		_ = f.Close()
	}
	loaded := len(c.entries)
	c.pruneLocked(time.Now().Add(-c.ttl).UnixMilli())
	c.enforceLimitLocked()
	if len(c.entries) < loaded {
		c.compactLocked()
		debug.FreeOSMemory()
	}
	log.Printf("loaded reasoning cache: entries=%d pruned=%d", len(c.entries), loaded-len(c.entries))
	go c.flushLoop()
	return c
}

func (c *reasoningCache) get(ids []string) (string, bool) {
	now := time.Now().UnixMilli()
	c.mu.RLock()
	for _, id := range ids {
		if e, ok := c.entries[id]; ok && e.Reasoning != "" {
			reasoning := e.Reasoning
			stale := e.LastSeen < now-cacheTouchInterval.Milliseconds()
			c.mu.RUnlock()
			if stale {
				c.touch(id, now)
			}
			return reasoning, true
		}
	}
	c.mu.RUnlock()
	return "", false
}

func (c *reasoningCache) touch(id string, now int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || e.LastSeen >= now-cacheTouchInterval.Milliseconds() {
		return
	}
	e.LastSeen = now
	c.entries[id] = e
	c.markDirtyLocked()
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
		old, ok := c.entries[id]
		if !ok || old.Reasoning != reasoning || old.LastSeen < now-cacheTouchInterval.Milliseconds() {
			c.entries[id] = cacheEntry{Reasoning: reasoning, LastSeen: now}
			changed = true
		}
	}
	if changed {
		c.pruneLocked(time.Now().Add(-c.ttl).UnixMilli())
		c.enforceLimitLocked()
		c.markDirtyLocked()
	}
}

func (c *reasoningCache) markDirtyLocked() {
	c.dirty = true
	c.version++
}

func (c *reasoningCache) pruneLocked(cutoff int64) {
	for id, e := range c.entries {
		if e.LastSeen < cutoff {
			delete(c.entries, id)
			c.markDirtyLocked()
		}
	}
}

func (c *reasoningCache) enforceLimitLocked() {
	if c.maxEntries <= 0 || len(c.entries) <= c.maxEntries {
		return
	}
	type candidate struct {
		id       string
		lastSeen int64
	}
	items := make([]candidate, 0, len(c.entries))
	for id, e := range c.entries {
		items = append(items, candidate{id: id, lastSeen: e.LastSeen})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].lastSeen < items[j].lastSeen })
	// Remove a batch so the expensive ordering is not repeated on every insert.
	target := c.maxEntries * 9 / 10
	if target < 1 {
		target = 1
	}
	for i := 0; len(c.entries) > target && i < len(items); i++ {
		delete(c.entries, items[i].id)
	}
	c.markDirtyLocked()
}

func (c *reasoningCache) compactLocked() {
	compact := make(map[string]cacheEntry, len(c.entries))
	for id, entry := range c.entries {
		compact[id] = entry
	}
	c.entries = compact
}

func (c *reasoningCache) flushLoop() {
	defer close(c.done)
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.flush(); err != nil {
				log.Printf("persist reasoning cache: %v", err)
			}
		case <-c.stop:
			if err := c.flush(); err != nil {
				log.Printf("persist reasoning cache during shutdown: %v", err)
			}
			return
		}
	}
}

func (c *reasoningCache) close() {
	close(c.stop)
	<-c.done
}

func (c *reasoningCache) flush() error {
	c.mu.RLock()
	if !c.dirty {
		c.mu.RUnlock()
		return nil
	}
	version := c.version
	if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		c.mu.RUnlock()
		return err
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		c.mu.RUnlock()
		return err
	}
	w := bufio.NewWriterSize(f, 256*1024)
	writeErr := c.writeJSONLocked(w)
	if writeErr == nil {
		writeErr = w.Flush()
	}
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	c.mu.RUnlock()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	c.mu.Lock()
	if c.version == version {
		c.dirty = false
	}
	count := len(c.entries)
	c.mu.Unlock()
	log.Printf("persisted reasoning cache: entries=%d", count)
	return nil
}

func (c *reasoningCache) writeJSONLocked(w io.Writer) error {
	if _, err := io.WriteString(w, "{"); err != nil {
		return err
	}
	first := true
	for id, entry := range c.entries {
		key, err := json.Marshal(id)
		if err != nil {
			return err
		}
		value, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		if _, err := w.Write(key); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ":"); err != nil {
			return err
		}
		if _, err := w.Write(value); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
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
	cacheTTL := flag.Duration("cache-ttl", defaultCacheTTL, "maximum age of unused cache entries")
	cacheMaxEntries := flag.Int("cache-max-entries", defaultCacheMaxEntries, "maximum reasoning cache entries")
	cacheFlushInterval := flag.Duration("cache-flush-interval", defaultCacheFlushInterval, "interval for batching cache writes")
	memoryLimitMiB := flag.Int64("memory-limit-mib", defaultMemoryLimitMiB, "Go soft memory limit in MiB (0 disables)")
	flag.Parse()
	if *memoryLimitMiB > 0 {
		debug.SetMemoryLimit(*memoryLimitMiB << 20)
	}
	target, err := url.Parse(*targetRaw)
	if err != nil {
		log.Fatal(err)
	}
	cache := loadCache(*cachePath, *cacheTTL, *cacheMaxEntries, *cacheFlushInterval)
	defer cache.close()
	p := &proxy{
		target: target,
		client: &http.Client{Timeout: 5 * time.Minute},
		cache:  cache,
	}
	log.Printf("reasoning compatibility proxy listening on %s -> %s", *listen, target)
	server := &http.Server{Addr: *listen, Handler: p}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(fmt.Errorf("serve: %w", err))
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown server: %v", err)
		}
	}
}
