package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
