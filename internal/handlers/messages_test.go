package handlers

import (
	"bytes"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type concurrentWriteDetector struct {
	header     http.Header
	body       bytes.Buffer
	active     int32
	concurrent int32
}

func (w *concurrentWriteDetector) Header() http.Header {
	return w.header
}

func (w *concurrentWriteDetector) WriteHeader(int) {}

func (w *concurrentWriteDetector) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.active, 1) != 1 {
		atomic.StoreInt32(&w.concurrent, 1)
	}
	defer atomic.AddInt32(&w.active, -1)
	time.Sleep(time.Millisecond)
	return w.body.Write(p)
}

func (w *concurrentWriteDetector) Flush() {
	if atomic.AddInt32(&w.active, 1) != 1 {
		atomic.StoreInt32(&w.concurrent, 1)
	}
	defer atomic.AddInt32(&w.active, -1)
	time.Sleep(time.Millisecond)
}

func TestResponseWriterSerializesStreamAndHeartbeatWrites(t *testing.T) {
	underlying := &concurrentWriteDetector{header: make(http.Header)}
	w := &responseWriter{ResponseWriter: underlying}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = w.Write([]byte("event: content_block_delta\ndata: {}\n\n"))
			w.Flush()
		}()
		go func() {
			defer wg.Done()
			_, _ = w.WriteAndFlush([]byte(":keepalive\n\n"))
		}()
	}
	wg.Wait()

	if atomic.LoadInt32(&underlying.concurrent) != 0 {
		t.Fatal("underlying ResponseWriter was accessed concurrently")
	}
}
