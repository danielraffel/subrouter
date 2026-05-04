package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin:       func(_ *http.Request) bool { return true },
	EnableCompression: true,
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", responses)
	mux.HandleFunc("/v1/models", models)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("mock upstream listening on %s", *addr)
	log.Fatal(server.ListenAndServe())
}

func responses(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		responsesWebSocket(w, r)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	log.Printf(
		"responses request method=%s auth=%s account_id=%q window=%q turn_state=%q bytes=%d",
		r.Method,
		redactBearer(r.Header.Get("Authorization")),
		r.Header.Get("ChatGPT-Account-ID"),
		r.Header.Get("x-codex-window-id"),
		r.Header.Get("x-codex-turn-state"),
		len(body),
	)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("x-codex-turn-state", "mock-turn-state")
	for _, event := range responseEvents() {
		fmt.Fprintf(w, "event: %s\n", eventType(event))
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
}

func responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	responseHeader := http.Header{}
	responseHeader.Set("x-codex-turn-state", "mock-turn-state")
	conn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Printf(
		"responses websocket auth=%s account_id=%q window=%q turn_state=%q",
		redactBearer(r.Header.Get("Authorization")),
		r.Header.Get("ChatGPT-Account-ID"),
		r.Header.Get("x-codex-window-id"),
		r.Header.Get("x-codex-turn-state"),
	)

	for {
		messageType, body, err := conn.ReadMessage()
		if err != nil {
			log.Printf("websocket read finished: %v", err)
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		log.Printf("responses websocket request bytes=%d", len(body))
		for _, event := range responseEvents() {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				log.Printf("websocket write failed: %v", err)
				return
			}
		}
	}
}

func models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"mock-model","object":"model","created":0,"owned_by":"subrouter"}]}`)
}

func responseEvents() []string {
	return []string{
		`{"type":"response.created","response":{"id":"resp_subrouter_smoke"}}`,
		`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_subrouter_smoke","content":[{"type":"output_text","text":"subrouter smoke ok"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_subrouter_smoke","usage":{"input_tokens":0,"input_tokens_details":null,"output_tokens":0,"output_tokens_details":null,"total_tokens":0}}}`,
	}
}

func eventType(json string) string {
	const marker = `"type":"`
	start := strings.Index(json, marker)
	if start < 0 {
		return "message"
	}
	start += len(marker)
	end := strings.IndexByte(json[start:], '"')
	if end < 0 {
		return "message"
	}
	return json[start : start+end]
}

func redactBearer(value string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	token := strings.TrimPrefix(value, prefix)
	if len(token) <= 10 {
		return prefix + "***"
	}
	return prefix + token[:6] + "..." + token[len(token)-4:]
}

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
