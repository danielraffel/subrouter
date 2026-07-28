package proxy

import (
	"io"
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
//
// Asserting on a total connection count is racy: the pool only gets a
// connection back once its body is closed, so a burst can legitimately dial an
// extra one or two. Instead, drive one warm-up burst, then repeat it and
// require that the second burst dials nothing. With a pool large enough for the
// concurrency, every connection is idle and reusable by then. With the default
// pool of 2, the other connections were discarded and must be re-dialed.
func TestOutboundTransportReusesConnectionsUnderConcurrency(t *testing.T) {
	const concurrency = 8

	var mu sync.Mutex
	conns := make(map[string]struct{})

	server := newTestServerCountingConns(t, &mu, conns)
	defer server.Close()

	client := &http.Client{Transport: NewOutboundTransport(), Timeout: 10 * time.Second}

	burst := func(round int) {
		var wg sync.WaitGroup
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				response, err := client.Get(server.URL)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				// Drain and close so the connection returns to the pool.
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}()
		}
		wg.Wait()
	}

	burst(1)
	mu.Lock()
	afterWarmup := len(conns)
	mu.Unlock()

	burst(2)
	mu.Lock()
	afterReuse := len(conns)
	mu.Unlock()

	if afterReuse != afterWarmup {
		t.Fatalf("second burst opened %d new connections (%d -> %d); a warm pool should have served all %d requests without dialing",
			afterReuse-afterWarmup, afterWarmup, afterReuse, concurrency)
	}
}
