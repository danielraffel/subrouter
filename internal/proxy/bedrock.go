package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// BedrockConfig configures the Bedrock signing gateway. When enabled, requests
// to /bedrock/* are re-signed with the team's AWS credentials (SigV4) and
// forwarded to bedrock-runtime, so clients (e.g. Claude Code in Bedrock gateway
// mode with CLAUDE_CODE_SKIP_BEDROCK_AUTH=1) never need AWS credentials.
type BedrockConfig struct {
	Region      string
	Credentials aws.CredentialsProvider
	// GatewayToken, when non-empty, must be presented by clients via the
	// Authorization: Bearer header (Claude Code's ANTHROPIC_AUTH_TOKEN). Empty
	// means the endpoint relies on network-level trust like the rest of the proxy.
	GatewayToken string
	Transport    http.RoundTripper
	// CostLogPath is the JSONL file where per-request token usage and estimated
	// cost are appended. Empty disables cost tracking.
	CostLogPath string
	// Bumper, when set, requests a Service Quotas increase when Bedrock throttles
	// (HTTP 429), deduped per quota with a cooldown.
	Bumper *bedrockQuotaBumper
}

const bedrockService = "bedrock"

func (s Server) bedrockHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Bedrock
		if cfg == nil || cfg.Credentials == nil || strings.TrimSpace(cfg.Region) == "" {
			http.Error(w, "bedrock gateway not configured", http.StatusServiceUnavailable)
			return
		}
		if cfg.GatewayToken != "" && !bedrockGatewayTokenOK(r, cfg.GatewayToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		upstreamPath := strings.TrimPrefix(r.URL.Path, "/bedrock")
		if upstreamPath == "" || upstreamPath == "/" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(upstreamPath, "/") {
			upstreamPath = "/" + upstreamPath
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		host := "bedrock-runtime." + cfg.Region + ".amazonaws.com"
		target := &url.URL{Scheme: "https", Host: host, Path: upstreamPath, RawQuery: r.URL.RawQuery}
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
			return
		}
		copyBedrockRequestHeaders(outReq.Header, r.Header)
		outReq.Host = host
		outReq.ContentLength = int64(len(body))

		creds, err := cfg.Credentials.Retrieve(r.Context())
		if err != nil {
			if s.Logger != nil {
				s.Logger.Error("bedrock credentials unavailable", "error", err)
			}
			http.Error(w, "bedrock credentials unavailable", http.StatusBadGateway)
			return
		}
		payloadHash := sha256Hex(body)
		signer := v4.NewSigner()
		if err := signer.SignHTTP(r.Context(), creds, outReq, payloadHash, bedrockService, cfg.Region, time.Now()); err != nil {
			if s.Logger != nil {
				s.Logger.Error("bedrock sigv4 signing failed", "error", err)
			}
			http.Error(w, "bedrock signing failed", http.StatusBadGateway)
			return
		}

		transport := cfg.Transport
		if transport == nil {
			transport = s.Transport
		}
		if transport == nil {
			transport = http.DefaultTransport
		}
		started := time.Now()
		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Error("bedrock upstream request failed", "path", upstreamPath, "error", err)
			}
			http.Error(w, "bedrock upstream request failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for key, values := range resp.Header {
			if isHopByHopHeader(key) {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)

		model := bedrockModelFromPath(upstreamPath)
		if resp.StatusCode == http.StatusTooManyRequests {
			if s.Logger != nil {
				s.Logger.Warn("bedrock throttled", "model", model, "path", upstreamPath)
			}
			if cfg.Bumper != nil {
				go cfg.Bumper.onThrottle(model)
			}
		}
		usage, haveUsage := s.streamBedrockResponse(w, resp)
		if cfg.CostLogPath != "" && model != "" {
			record := bedrockCostRecord{
				Timestamp:  started.UTC().Format(time.RFC3339),
				Model:      model,
				Status:     resp.StatusCode,
				DurationMs: time.Since(started).Milliseconds(),
			}
			if haveUsage {
				record.Usage = usage
				record.CostUSD = usage.costUSD(model)
			}
			appendBedrockCostRecord(cfg.CostLogPath, record)
		}
	})
}

const bedrockFableModelID = "us.anthropic.claude-fable-5"

// maybeServeClaudeFableViaBedrock intercepts Claude Messages requests for a
// Fable model and serves them from Bedrock instead of the OAuth subscription
// pool. Fable is entitlement-gated on the claude.ai subscription (Anthropic
// returns 429 even at 0% usage), so the only working path is Bedrock. Returns
// true if it handled the response. For any non-Fable request it restores the
// request body and returns false so the normal proxy path runs unchanged.
func (s Server) maybeServeClaudeFableViaBedrock(w http.ResponseWriter, r *http.Request) bool {
	cfg := s.Bedrock
	if cfg == nil || cfg.Credentials == nil || strings.TrimSpace(cfg.Region) == "" {
		return false
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return false
	}
	restore := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil || !strings.Contains(strings.ToLower(probe.Model), "fable") {
		restore()
		return false
	}
	s.serveClaudeFableViaBedrock(w, r, body)
	return true
}

func (s Server) serveClaudeFableViaBedrock(w http.ResponseWriter, r *http.Request, body []byte) {
	cfg := s.Bedrock
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	stream := false
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	// Bedrock takes the model in the URL and streaming via the endpoint; the body
	// must carry anthropic_version and must not carry model/stream.
	delete(payload, "model")
	delete(payload, "stream")
	payload["anthropic_version"] = json.RawMessage(`"bedrock-2023-05-31"`)
	newBody, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to build bedrock request", http.StatusInternalServerError)
		return
	}

	endpoint := "invoke"
	if stream {
		endpoint = "invoke-with-response-stream"
	}
	path := "/model/" + bedrockFableModelID + "/" + endpoint
	started := time.Now()
	resp, err := s.signAndForwardBedrock(r.Context(), http.MethodPost, path, newBody)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("claude-fable bedrock request failed", "error", err)
		}
		http.Error(w, "bedrock upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		if s.Logger != nil {
			s.Logger.Warn("bedrock throttled", "model", bedrockFableModelID, "path", path)
		}
		if cfg.Bumper != nil {
			go cfg.Bumper.onThrottle(bedrockFableModelID)
		}
	}

	var usage bedrockUsage
	var haveUsage bool
	if stream && resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		usage, haveUsage = transcodeBedrockToSSE(w, resp.Body)
	} else {
		// Non-stream success or any error: Bedrock returns Anthropic-format JSON.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, replayablePostMaxBodyBytes))
		_, _ = w.Write(respBody)
		usage, haveUsage = parseBedrockInvokeUsage(respBody)
	}
	if cfg.CostLogPath != "" {
		record := bedrockCostRecord{
			Timestamp:  started.UTC().Format(time.RFC3339),
			Model:      bedrockFableModelID,
			Status:     resp.StatusCode,
			DurationMs: time.Since(started).Milliseconds(),
		}
		if haveUsage {
			record.Usage = usage
			record.CostUSD = usage.costUSD(bedrockFableModelID)
		}
		appendBedrockCostRecord(cfg.CostLogPath, record)
	}
}

// signAndForwardBedrock SigV4-signs a JSON body to bedrock-runtime and returns
// the raw response.
func (s Server) signAndForwardBedrock(ctx context.Context, method, upstreamPath string, body []byte) (*http.Response, error) {
	cfg := s.Bedrock
	host := "bedrock-runtime." + cfg.Region + ".amazonaws.com"
	target := &url.URL{Scheme: "https", Host: host, Path: upstreamPath}
	outReq, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	outReq.Header.Set("Content-Type", "application/json")
	outReq.Host = host
	outReq.ContentLength = int64(len(body))
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, outReq, sha256Hex(body), bedrockService, cfg.Region, time.Now()); err != nil {
		return nil, err
	}
	transport := cfg.Transport
	if transport == nil {
		transport = s.Transport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(outReq)
}

// transcodeBedrockToSSE converts a Bedrock invoke-with-response-stream body (AWS
// event-stream framing wrapping Anthropic event JSON) into Anthropic Messages
// SSE, which is what a Claude client on the OAuth path expects. It also extracts
// token usage for cost tracking.
func transcodeBedrockToSSE(w http.ResponseWriter, src io.Reader) (bedrockUsage, bool) {
	flusher, _ := w.(http.Flusher)
	var scanner bedrockFrameScanner
	var usage bedrockUsage
	var got bool
	emit := func(inner []byte) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage bedrockUsage `json:"usage"`
			} `json:"message"`
			Usage bedrockUsage `json:"usage"`
		}
		_ = json.Unmarshal(inner, &ev)
		switch ev.Type {
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
			usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
			usage.CacheWriteTokens = ev.Message.Usage.CacheWriteTokens
			got = true
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
				got = true
			}
		}
		eventType := ev.Type
		if eventType == "" {
			eventType = "message"
		}
		_, _ = w.Write([]byte("event: " + eventType + "\ndata: "))
		_, _ = w.Write(inner)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			scanner.feed(buf[:n], emit)
		}
		if readErr != nil {
			break
		}
	}
	return usage, got && !usage.empty()
}

// bedrockFrameScanner parses AWS event-stream frames incrementally and yields
// each frame's decoded inner payload (the Anthropic event JSON).
type bedrockFrameScanner struct{ buf []byte }

func (s *bedrockFrameScanner) feed(data []byte, emit func([]byte)) {
	s.buf = append(s.buf, data...)
	for {
		if len(s.buf) < 12 {
			return
		}
		total := int(binary.BigEndian.Uint32(s.buf[0:4]))
		headersLen := int(binary.BigEndian.Uint32(s.buf[4:8]))
		if total < 16 || total > 64<<20 {
			s.buf = nil
			return
		}
		if len(s.buf) < total {
			return
		}
		payloadStart := 12 + headersLen
		payloadEnd := total - 4
		if payloadStart <= payloadEnd && payloadEnd <= len(s.buf) {
			var wrap struct {
				Bytes string `json:"bytes"`
			}
			if json.Unmarshal(s.buf[payloadStart:payloadEnd], &wrap) == nil && wrap.Bytes != "" {
				if decoded, err := base64.StdEncoding.DecodeString(wrap.Bytes); err == nil {
					emit(decoded)
				}
			}
		}
		s.buf = s.buf[total:]
	}
}

func (s Server) handleBedrockCost(w http.ResponseWriter, r *http.Request) {
	path := ""
	if s.Bedrock != nil {
		path = s.Bedrock.CostLogPath
	}
	writeJSON(w, summarizeBedrockCost(path))
}

func bedrockGatewayTokenOK(r *http.Request, token string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):]) == token
	}
	return auth == token
}

// copyBedrockRequestHeaders forwards client headers that Bedrock needs while
// dropping hop-by-hop headers, the client's own Authorization (we re-sign), and
// any pre-existing AWS signing headers.
func copyBedrockRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || lower == "authorization" || lower == "host" || lower == "content-length" {
			continue
		}
		if strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// bedrockModelFromPath extracts the model id from /model/<id>/invoke[...] paths.
func bedrockModelFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 2 && parts[0] == "model" {
		return parts[1]
	}
	return ""
}

// streamBedrockResponse forwards the upstream response to the client while
// extracting token usage. Event-stream responses are parsed frame-by-frame as
// they flow; non-streaming JSON responses are buffered (they are small) and
// parsed directly. Usage extraction never blocks or corrupts the forwarded
// bytes.
func (s Server) streamBedrockResponse(w http.ResponseWriter, resp *http.Response) (bedrockUsage, bool) {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "eventstream") {
		parser := newBedrockStreamUsageWriter()
		flushingCopy(w, resp.Body, parser)
		return parser.Usage()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, replayablePostMaxBodyBytes))
	if err == nil && len(body) > 0 {
		_, _ = w.Write(body)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return parseBedrockInvokeUsage(body)
}

// flushingCopy streams src to the client, flushing after each chunk, and
// mirrors the bytes to sink (used for usage parsing) when sink is non-nil.
func flushingCopy(w http.ResponseWriter, src io.Reader, sink io.Writer) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if sink != nil {
				_, _ = sink.Write(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding",
		"te", "trailer", "upgrade", "proxy-authenticate", "proxy-authorization":
		return true
	}
	return false
}
