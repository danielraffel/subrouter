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
		flushingCopy(w, resp.Body)
	})
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

func flushingCopy(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
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
