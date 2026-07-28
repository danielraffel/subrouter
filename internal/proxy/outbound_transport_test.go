package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newTestServerCountingConns records every distinct client connection the
// server accepts, so a test can tell reuse from per-request dialing.
func newTestServerCountingConns(t *testing.T, mu *sync.Mutex, conns map[string]struct{}) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		mu.Lock()
		conns[c.RemoteAddr().String()] = struct{}{}
		mu.Unlock()
	}
	server.Start()
	return server
}

// The proxy speaks HTTP/1.1 only (see NewOutboundTransport), so concurrency is
// bounded by connection count rather than stream multiplexing. If the idle pool
// is left at Go's default of 2, concurrent requests to one host burn a fresh
// connection each and strand it in TIME_WAIT, exhausting ephemeral ports.
func TestNewOutboundTransportPoolsConnectionsPerHost(t *testing.T) {
	transport := NewOutboundTransport()

	if transport.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want more than the default %d; HTTP/1.1 cannot multiplex so a small pool forces one dial per concurrent request",
			transport.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConns = %d, want >= MaxIdleConnsPerHost %d, otherwise the global cap silently defeats the per-host pool",
			transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout must be positive so idle connections are eventually reclaimed")
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must stay false; the pool sizing above assumes one request per connection")
	}
}

// Concurrent requests to one host must reuse connections rather than dial per
// request. This is the behavior whose absence exhausted the port range.
func TestOutboundTransportReusesConnectionsUnderConcurrency(t *testing.T) {
	var mu sync.Mutex
	conns := make(map[string]struct{})

	server := newTestServerCountingConns(t, &mu, conns)
	defer server.Close()

	transport := NewOutboundTransport()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	// Two sequential rounds: the second must reuse the first round's pool.
	for round := range 2 {
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				response, err := client.Get(server.URL)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				_ = response.Body.Close()
			}()
		}
		wg.Wait()
	}

	mu.Lock()
	distinct := len(conns)
	mu.Unlock()

	// 16 requests over 8-way concurrency should not open 16 connections.
	if distinct > 8 {
		t.Fatalf("opened %d distinct connections for 16 requests; connections are not being reused", distinct)
	}
}
