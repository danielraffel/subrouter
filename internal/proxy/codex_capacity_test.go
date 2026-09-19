package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCodexCapacityBody(t *testing.T) {
	capacity := []string{
		`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`,
		`{"error":{"code":"model_at_capacity"}}`,
		`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`,
		`{"detail":"The model you selected is at capacity, retry soon"}`,
	}
	for _, body := range capacity {
		if !codexCapacityBody([]byte(body)) {
			t.Fatalf("capacity body not detected: %s", body)
		}
	}
	notCapacity := []string{
		`{"error":{"code":"usage_limit_reached"}}`,
		`{"error":{"code":"context_length_exceeded"}}`,
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"You have hit your usage limit."}}`,
	}
	for _, body := range notCapacity {
		if codexCapacityBody([]byte(body)) {
			t.Fatalf("non-capacity body misclassified: %s", body)
		}
	}
}

func TestCodexCapacityBackoffAt(t *testing.T) {
	now := time.Now()
	if wait := codexCapacityBackoffAt(http.Header{}, 0, now); wait != time.Second {
		t.Fatalf("first backoff = %v", wait)
	}
	if wait := codexCapacityBackoffAt(http.Header{}, 2, now); wait != 4*time.Second {
		t.Fatalf("third backoff = %v", wait)
	}
	if wait := codexCapacityBackoffAt(http.Header{}, 20, now); wait != codexCapacityMaxWait {
		t.Fatalf("capped backoff = %v", wait)
	}
	header := http.Header{"Retry-After": {"2"}}
	if wait := codexCapacityBackoffAt(header, 0, now); wait != 2*time.Second {
		t.Fatalf("retry-after backoff = %v", wait)
	}
	header = http.Header{"Retry-After": {"600"}}
	if wait := codexCapacityBackoffAt(header, 0, now); wait != codexCapacityRetryAfterMaxWait {
		t.Fatalf("retry-after cap = %v", wait)
	}
}

// peekBodiesMatch reads the (rebuilt) response body and compares it to the
// original stream: inspection must never eat a byte.
func peekBodiesMatch(t *testing.T, response *http.Response, want string) {
	t.Helper()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("rebuilt body = %q, want %q", got, want)
	}
}

func TestPeekCodexBootstrapFailure(t *testing.T) {
	handshake := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n" +
		"data: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i1\"}}\n\n"
	overload := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n"
	quota := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\"}}}\n\n"
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n"

	sse := func(body string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	t.Run("overload after handshake", func(t *testing.T) {
		stream := handshake + overload
		class, _, err := peekCodexBootstrapFailure(sse(stream))
		if err != nil || class != codexFailureServer {
			t.Fatalf("class = %v, err %v", class, err)
		}
	})

	t.Run("quota after handshake", func(t *testing.T) {
		stream := handshake + quota
		class, _, err := peekCodexBootstrapFailure(sse(stream))
		if err != nil || class != codexFailureQuota {
			t.Fatalf("class = %v, err %v", class, err)
		}
	})

	t.Run("rebuilt body is byte identical", func(t *testing.T) {
		stream := handshake + overload
		response := sse(stream)
		if _, _, err := peekCodexBootstrapFailure(response); err != nil {
			t.Fatal(err)
		}
		peekBodiesMatch(t, response, stream)
	})

	t.Run("output delta commits the stream", func(t *testing.T) {
		// A failure after meaningful output must not be classified: the
		// request is never replayed once the client could have seen output.
		stream := handshake + delta + overload
		response := sse(stream)
		class, _, err := peekCodexBootstrapFailure(response)
		if err != nil || class != codexFailureNone {
			t.Fatalf("class = %v, err %v", class, err)
		}
		peekBodiesMatch(t, response, stream)
	})

	t.Run("successful stream", func(t *testing.T) {
		stream := handshake + delta + completed
		class, _, err := peekCodexBootstrapFailure(sse(stream))
		if err != nil || class != codexFailureNone {
			t.Fatalf("class = %v, err %v", class, err)
		}
	})

	t.Run("non-stream failure json", func(t *testing.T) {
		body := `{"status":"failed","error":{"code":"server_is_overloaded"}}`
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		class, _, err := peekCodexBootstrapFailure(response)
		if err != nil || class != codexFailureServer {
			t.Fatalf("class = %v, err %v", class, err)
		}
		peekBodiesMatch(t, response, body)
	})

	t.Run("non-stream success json", func(t *testing.T) {
		body := `{"status":"completed","output":[]}`
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		class, _, err := peekCodexBootstrapFailure(response)
		if err != nil || class != codexFailureNone {
			t.Fatalf("class = %v, err %v", class, err)
		}
	})
}

// scriptedCodexTransport answers RoundTrip calls from a script, one fresh
// response per call.
type scriptedCodexTransport struct {
	mu      sync.Mutex
	calls   int
	handler func(call int) *http.Response
}

func (s *scriptedCodexTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.handler(s.calls), nil
}

func codexCapacityTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := makeRequestBodyReplayable(req, replayablePostMaxBodyBytes); err != nil || !ok {
		t.Fatalf("replayable body = %v, %v", ok, err)
	}
	return req
}

func codexSSEResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCodexCapacityRetry5xxSameAccountThenSucceeds(t *testing.T) {
	success := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		if call == 1 {
			return codexSSEResponse(http.StatusServiceUnavailable, "capacity")
		}
		return codexSSEResponse(http.StatusOK, success)
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: recordSleep(&waits), budget: newAttemptBudget(5),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || script.calls != 2 {
		t.Fatalf("status = %d, calls = %d", res.StatusCode, script.calls)
	}
	peekBodiesMatch(t, res, success)
	if len(waits) != 1 {
		t.Fatalf("waits = %v", waits)
	}
}

func TestCodexCapacityRetryInStreamOverloadThenSucceeds(t *testing.T) {
	failure := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"
	success := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		if call == 1 {
			return codexSSEResponse(http.StatusOK, failure)
		}
		return codexSSEResponse(http.StatusOK, success)
	}}
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: recordSleep(&[]time.Duration{}), budget: newAttemptBudget(5),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || script.calls != 2 {
		t.Fatalf("status = %d, calls = %d", res.StatusCode, script.calls)
	}
	// The smuggled failure never reaches the client; the retried stream does.
	peekBodiesMatch(t, res, success)
}

func TestCodexCapacityRetryBudgetExhaustedPassesThrough(t *testing.T) {
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		return codexSSEResponse(http.StatusServiceUnavailable, "capacity")
	}}
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: recordSleep(&[]time.Duration{}), budget: newAttemptBudget(2),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if script.calls != 3 { // initial attempt + 2 budgeted retries
		t.Fatalf("calls = %d", script.calls)
	}
}

func TestCodexCapacityRetryHonorsRetryAfter(t *testing.T) {
	now := time.Now()
	var waits []time.Duration
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		if call == 1 {
			res := codexSSEResponse(http.StatusServiceUnavailable, "capacity")
			res.Header.Set("Retry-After", "2")
			return res
		}
		return codexSSEResponse(http.StatusOK, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
	}}
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
		budget: newAttemptBudget(5),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, err %v", res.StatusCode, err)
	}
	if len(waits) != 1 || waits[0] < time.Second || waits[0] > 3*time.Second {
		t.Fatalf("waits = %v (started %v)", waits, now)
	}
}

func TestCodexCapacityNoReplayAfterOutputStarted(t *testing.T) {
	// A failure that arrives after meaningful output is delivered in-stream
	// exactly as sent; the request is not retried.
	stream := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		return codexSSEResponse(http.StatusOK, stream)
	}}
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: recordSleep(&[]time.Duration{}), budget: newAttemptBudget(5),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if script.calls != 1 {
		t.Fatalf("calls = %d (stream with output must not be replayed)", script.calls)
	}
	peekBodiesMatch(t, res, stream)
}

func TestCodexCapacity4xxMessageRetried(t *testing.T) {
	// Codex sometimes reports capacity as HTTP 400 with only the message text.
	success := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	script := &scriptedCodexTransport{handler: func(call int) *http.Response {
		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)),
			}
		}
		return codexSSEResponse(http.StatusOK, success)
	}}
	transport := usageLimitRetryTransport{
		base: script, provider: accounts.ProviderCodex, maxAttempts: 6,
		sleep: recordSleep(&[]time.Duration{}), budget: newAttemptBudget(5),
	}
	res, err := transport.RoundTrip(codexCapacityTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || script.calls != 2 {
		t.Fatalf("status = %d, calls = %d", res.StatusCode, script.calls)
	}
}

func TestCodexUsageLimit429StillFailsOver(t *testing.T) {
	// The capacity path must not swallow the existing 429 usage-limit class.
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`))}
	capacity, err := sniffCodexCapacityResponse(response)
	if err != nil || capacity {
		t.Fatalf("429 rate limit classified as capacity: %v, %v", capacity, err)
	}
}
