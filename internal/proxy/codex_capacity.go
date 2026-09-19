package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Codex model-capacity failures ("Selected model is at capacity. Please try
// a different model.", server_is_overloaded) arrive in three shapes:
//
//  1. A bare 5xx status.
//  2. A capacity message inside a 4xx body — Codex sometimes reports capacity
//     as HTTP 400 instead of 503.
//  3. Most commonly, a terminal response.failed/error event smuggled inside
//     an HTTP 200 SSE stream right after the handshake events, instead of a
//     503 on the wire.
//
// usageLimitRetryTransport intercepts all three before the client sees a
// byte and retries — the same account first (capacity often clears in
// seconds and the prompt cache stays warm), then another account with a
// short pool-scoped cooldown on the one that failed, with exponential
// backoff capped below. Once output has started the request is never
// replayed; the failure is delivered in-stream exactly as the upstream sent
// it.

const (
	// codexCapacityMaxRetries bounds capacity retries per request (same-account
	// retries and account rotations combined), independent of the failover
	// attempt slots, so a sustained capacity storm cannot loop a request
	// forever. The shared request budget usually binds first.
	codexCapacityMaxRetries = 8
	// codexCapacityMaxWait caps one capacity backoff wait; Retry-After is
	// honored further out because the upstream asked for it explicitly.
	codexCapacityMaxWait            = 30 * time.Second
	codexCapacityRetryAfterMaxWait  = 60 * time.Second
	codexCapacityCooldownTTL        = time.Minute
	codexBootstrapInspectMaxBytes   = 1 << 20
	codexBootstrapInspectMaxRecords = 512
)

// codexCapacityBackoffAt picks the wait before a capacity retry: Retry-After
// when the upstream sent one (capped higher), else 1s << retry (1s, 2s, 4s,
// ...) capped at codexCapacityMaxWait.
func codexCapacityBackoffAt(header http.Header, retry int, now time.Time) time.Duration {
	if retryAt := parseRetryAfter(strings.TrimSpace(claudeHeaderGet(header, "Retry-After")), now); !retryAt.IsZero() {
		wait := retryAt.Sub(now)
		if wait > codexCapacityRetryAfterMaxWait {
			return codexCapacityRetryAfterMaxWait
		}
		return wait
	}
	wait := time.Second << retry
	if wait > codexCapacityMaxWait {
		return codexCapacityMaxWait
	}
	return wait
}

// codexCapacityBody reports whether an error body describes model capacity
// rather than quota or a client fault. Codex does not always use a distinct
// code, so the message text is the signal (as in CLIProxyAPI's classifier).
func codexCapacityBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "model is at capacity") ||
		strings.Contains(lower, "model_at_capacity") ||
		strings.Contains(lower, "model_is_at_capacity") ||
		strings.Contains(lower, "server_is_overloaded") ||
		strings.Contains(lower, "service_unavailable_error") ||
		strings.Contains(lower, "our servers are currently overloaded") {
		return true
	}
	return strings.Contains(lower, "model") && strings.Contains(lower, "at capacity")
}

// sniffCodexCapacityResponse reads a non-stream error body's prefix (leaving
// the response deliverable) and reports whether it is a capacity rejection.
func sniffCodexCapacityResponse(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return codexCapacityBody(prefix), nil
}

// peekCodexBootstrapFailure inspects the start of a Codex 200 response while
// the request can still be replayed elsewhere: the upstream delivers overload
// rejections (and, more rarely, quota failures) inside an HTTP 200 stream
// right after the handshake events instead of putting a failure status on
// the wire. Handshake frames are held until the first meaningful output
// event, a terminal failure event, or the inspection budgets; a non-streamed
// response is classified from its single JSON document. On a failure event
// the payload is returned alongside the class so the caller can route
// special cases (model incompatibility) to their own paths. The body is
// always rebuilt, so a delivered response is byte-identical to what the
// upstream sent, and a stream that already produced meaningful output is
// never held or replayed.
func peekCodexBootstrapFailure(response *http.Response) (codexFailureClass, []byte, error) {
	if response == nil || response.Body == nil {
		return codexFailureNone, nil, nil
	}
	body := response.Body
	rebuild := func(buffered []byte) {
		response.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(buffered), body),
			Closer: body,
		}
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/event-stream") {
		prefix, err := io.ReadAll(io.LimitReader(body, codexBootstrapInspectMaxBytes+1))
		if err != nil || int64(len(prefix)) > codexBootstrapInspectMaxBytes {
			rebuild(prefix)
			return codexFailureNone, nil, err
		}
		rebuild(prefix)
		if class := codexTurnFailureClass(prefix); class != codexFailureNone {
			return class, prefix, nil
		}
		return codexFailureNone, nil, nil
	}
	reader := bufio.NewReader(body)
	var buffered bytes.Buffer
	// bufio prefetches past the lines the loop consumes, so rebuilding must
	// first drain its internal buffer or those bytes would vanish from the
	// delivered stream.
	rebuildStream := func() {
		if n := reader.Buffered(); n > 0 {
			if tail, err := reader.Peek(n); err == nil {
				buffered.Write(tail)
			}
		}
		rebuild(buffered.Bytes())
	}
	for records := 0; buffered.Len() <= codexBootstrapInspectMaxBytes && records < codexBootstrapInspectMaxRecords; records++ {
		line, err := reader.ReadBytes('\n')
		buffered.Write(line)
		if err != nil {
			// EOF or a read error ends inspection; whatever was buffered is
			// delivered as-is.
			rebuildStream()
			if err == io.EOF {
				return codexFailureNone, nil, nil
			}
			return codexFailureNone, nil, err
		}
		payload, ok := sseDataPayload(line)
		if !ok {
			continue
		}
		if class := codexTurnFailureClass(payload); class != codexFailureNone {
			rebuildStream()
			return class, payload, nil
		}
		if eventType := sseEventType(payload); eventType == "" || codexBootstrapEventMeaningful(eventType) {
			rebuildStream()
			return codexFailureNone, nil, nil
		}
	}
	rebuildStream()
	return codexFailureNone, nil, nil
}

// sseDataPayload extracts the JSON payload of a single-line SSE data record.
// Multi-line data framing, comments, and event:/id: lines are not payloads.
func sseDataPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

// sseEventType reads the "type" field of an SSE data payload without a full
// document parse; failure classification already ran on the payload by now.
func sseEventType(payload []byte) string {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringField(event, "type")))
}

// codexBootstrapEventMeaningful reports whether an SSE event proves the
// upstream committed to serving the turn: any generated content, or the
// stream's successful end. Handshake events (the response shell, empty
// item/part announcements) do not — a capacity rejection typically arrives
// immediately after them.
func codexBootstrapEventMeaningful(eventType string) bool {
	switch eventType {
	case "response.created",
		"response.in_progress",
		"response.queued",
		"response.output_item.added",
		"response.content_part.added",
		"response.reasoning_summary_part.added":
		return false
	default:
		return true
	}
}
