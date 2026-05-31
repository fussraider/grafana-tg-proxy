package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"grafana-tg-proxy/internal/config"
	"grafana-tg-proxy/internal/db"
	"grafana-tg-proxy/internal/logger"
	"grafana-tg-proxy/internal/metrics"
	"grafana-tg-proxy/internal/proxy"
)

type contextKey string

const bodyKey contextKey = "requestBody"

// Server handles the HTTP listener and reverse proxy routing.
type Server struct {
	cfg      *config.Config
	rotator  *proxy.Rotator
	database *db.DB
	log      *logger.Logger
	rp       *httputil.ReverseProxy
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config, rot *proxy.Rotator, database *db.DB) *Server {
	log := logger.New("http")

	// Set up the reverse proxy engine
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Redirect request to Telegram API
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = "api.telegram.org"
			pr.Out.Host = "api.telegram.org"
			
			// Remove headers that might leak internal network info
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del("X-Forwarded-Host")
		},
		Transport: rot.GetHTTPClient().Transport,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusOK {
				route := "socks5"
				if len(rot.GetProxiesStatus()) == 0 {
					route = "direct"
				}
				metrics.IncAlertsDelivered(route, "initial")
			} else {
				metrics.IncAlertsFailed(fmt.Sprintf("http_%d", resp.StatusCode))
			}
			return nil
		},
		ErrorLog:  nil, // Suppress default stdlog output since we handle errors explicitly
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("Reverse proxy failed to deliver request %s: %v", r.URL.Path, err)
			
			// Intercept and persist failing alert deliveries (POST requests to /sendMessage)
			if strings.HasSuffix(r.URL.Path, "/sendMessage") && r.Method == http.MethodPost {
				bodyBytes, _ := r.Context().Value(bodyKey).([]byte)
				token := extractToken(r.URL.Path)

				if token != "" && len(bodyBytes) > 0 {
					payloadStr := string(bodyBytes)
					chatID := extractChatID(payloadStr)

					// Spool alert to SQLite database
					dbErr := database.SaveAlert(token, chatID, payloadStr, r.URL.RawQuery, time.Now())
					if dbErr == nil {
						metrics.IncAlertsFailed("spooled")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusAccepted)
						w.Write([]byte(`{"ok":true,"description":"Alert accepted: spooled to local SQLite queue for retry due to transmission failure."}`))
						return
					}
					log.Error("Failed to spool alert to SQLite: %v", dbErr)
				}
			}

			metrics.IncAlertsFailed("network_error")
			// Return a 502 Bad Gateway if it's not a sendMessage alert or database spooling failed
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(fmt.Sprintf(`{"ok":false,"description":"Bad Gateway: proxy failed to relay connection: %v"}`, err)))
		},
	}

	return &Server{
		cfg:      cfg,
		rotator:  rot,
		database: database,
		log:      log,
		rp:       rp,
	}
}

// Start boots the HTTP listener and handles routing endpoints.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register status/healthcheck endpoint
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Catch-all route to handle Telegram proxy prefix
	mux.HandleFunc("/", s.handleCatchAll)

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	s.log.Info("Proxy server listening on http://localhost%s", addr)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 35 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return server.ListenAndServe()
}

func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/bot") {
		s.handleProxy(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","version":"0.1.0"}`))
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Redact path logging for security
	maskedPath := logger.Redact(r.URL.Path)
	s.log.Info("Received Telegram request: %s %s from client %s", r.Method, maskedPath, r.RemoteAddr)

	if strings.HasSuffix(r.URL.Path, "/sendMessage") && r.Method == http.MethodPost {
		metrics.IncAlertsReceived()
	}

	start := time.Now()

	// Extract and clone request body to save in request Context for spooling fallback
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.log.Error("Failed to read request body: %v", err)
		http.Error(w, `{"ok":false,"description":"Bad Request: failed to read payload"}`, http.StatusBadRequest)
		return
	}

	// Auto-detect and translate Grafana Webhook payloads (pre-Grafana 12 format)
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sendMessage") {
		var gWebhook struct {
			Receiver string `json:"receiver"`
			Status   string `json:"status"`
			Title    string `json:"title"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(bodyBytes, &gWebhook); err == nil && (gWebhook.Receiver != "" || gWebhook.Status != "") {
			chatID := r.URL.Query().Get("chat_id")
			if chatID == "" {
				s.log.Error("Failed to translate Grafana webhook: chat_id query parameter is missing")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"ok":false,"description":"Bad Request: chat_id query parameter is required in the URL for Grafana webhook translation"}`))
				return
			}

			// Format dynamic text from Grafana payload with safety escaping and markdown parsing
			titleText := ""
			if gWebhook.Title != "" {
				titleText = "<b>" + html.EscapeString(gWebhook.Title) + "</b>"
			}
			
			messageText := ""
			if gWebhook.Message != "" {
				messageText = translateMarkdownToHTML(gWebhook.Message)
			}
			
			text := ""
			if titleText != "" {
				text = titleText
			}
			if messageText != "" {
				if text != "" {
					text += "\n\n"
				}
				text += messageText
			}
			if text == "" {
				text = "Grafana Alert Notification"
			}

			tgPayload := map[string]any{
				"chat_id":    chatID,
				"text":       text,
				"parse_mode": "HTML",
			}
			translatedBytes, err := json.Marshal(tgPayload)
			if err != nil {
				s.log.Error("Failed to marshal translated Telegram payload: %v", err)
			} else {
				s.log.Info("Successfully translated Grafana webhook payload to Telegram payload for chat_id: %s", chatID)
				bodyBytes = translatedBytes
			}
		}
	}
	
	// Restore body stream for reverse proxy reading
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))

	// Store bytes inside request context
	ctx := context.WithValue(r.Context(), bodyKey, bodyBytes)
	r = r.WithContext(ctx)
	
	// Pass to reverse proxy engine
	s.rp.ServeHTTP(w, r)
	
	duration := time.Since(start)
	s.log.Debug("Completed Telegram request relay in %v", duration)
}

func extractToken(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && strings.HasPrefix(parts[1], "bot") {
		return strings.TrimPrefix(parts[1], "bot")
	}
	return ""
}

func extractChatID(payload string) string {
	// 1. Try parsing JSON
	var jsonMap map[string]any
	if err := json.Unmarshal([]byte(payload), &jsonMap); err == nil {
		if chatID, ok := jsonMap["chat_id"].(string); ok {
			return chatID
		}
		if chatIDNum, ok := jsonMap["chat_id"].(float64); ok {
			return fmt.Sprintf("%.0f", chatIDNum)
		}
	}
	// 2. Try parsing form values
	if values, err := url.ParseQuery(payload); err == nil {
		if chatID := values.Get("chat_id"); chatID != "" {
			return chatID
		}
	}
	return "unknown"
}

func translateMarkdownToHTML(s string) string {
	// 1. Escape standard HTML characters to make it safe for Telegram's HTML parser
	escaped := html.EscapeString(s)

	// 2. Convert **bold** to <b>bold</b>
	boldParts := strings.Split(escaped, "**")
	if len(boldParts) >= 3 {
		var builder strings.Builder
		for i, part := range boldParts {
			builder.WriteString(part)
			if i < len(boldParts)-1 {
				if i%2 == 0 {
					builder.WriteString("<b>")
				} else {
					builder.WriteString("</b>")
				}
			}
		}
		escaped = builder.String()
	}

	// 3. Convert `code` to <code>code</code>
	codeParts := strings.Split(escaped, "`")
	if len(codeParts) >= 3 {
		var builder strings.Builder
		for i, part := range codeParts {
			builder.WriteString(part)
			if i < len(codeParts)-1 {
				if i%2 == 0 {
					builder.WriteString("<code>")
				} else {
					builder.WriteString("</code>")
				}
			}
		}
		escaped = builder.String()
	}

	return escaped
}
