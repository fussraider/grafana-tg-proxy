package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"grafana-tg-proxy/internal/config"
	"grafana-tg-proxy/internal/proxy"
)

func TestHealthzEndpoint(t *testing.T) {
	cfg := &config.Config{Port: 8080}
	rot, _ := proxy.NewRotator([]string{}, 1*time.Second, true)
	s := NewServer(cfg, rot, nil)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var data map[string]string
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}

	if data["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", data["status"])
	}
}

func TestProxyRelay(t *testing.T) {
	// 1. Start a mock Telegram API server
	mockTelegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request details arrived at Telegram
		if r.Method != "POST" {
			t.Errorf("expected method POST at Telegram, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/bot12345:token/sendMessage") {
			t.Errorf("unexpected URL path at Telegram: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer mockTelegram.Close()

	// 2. Configure our proxy rotator to dial the mock Telegram server directly.
	// Since we mock the reverse proxy target, we will configure the proxy's transport rewrite host to mockTelegram's address.
	cfg := &config.Config{Port: 8080}
	rot, _ := proxy.NewRotator([]string{}, 1*time.Second, true)
	s := NewServer(cfg, rot, nil)

	// Override reverse proxy rewrite behavior to hit our mock local server instead of api.telegram.org
	s.rp.Rewrite = func(pr *httputil.ProxyRequest) {
		targetURL := mockTelegram.URL
		// Parse mock url host/scheme
		u, _ := pr.Out.URL.Parse(targetURL)
		pr.Out.URL.Scheme = u.Scheme
		pr.Out.URL.Host = u.Host
		pr.Out.Host = u.Host
	}

	// 3. Create test server handler
	handler := http.NewServeMux()
	handler.HandleFunc("/", s.handleCatchAll)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 4. Send request to our proxy server
	payload := `{"chat_id":"123","text":"firing"}`
	resp, err := http.Post(ts.URL+"/bot12345:token/sendMessage", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("failed to send request to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected proxy response 200 OK, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse proxy response: %v", err)
	}

	if result["ok"] != true {
		t.Errorf("expected result ok: true, got %+v", result)
	}
}

func TestGrafanaWebhookTranslation(t *testing.T) {
	// 1. Start mock Telegram API server that checks if the request body was successfully translated to standard Telegram format
	mockTelegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body at mock Telegram: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("failed to parse JSON at mock Telegram: %v", err)
		}

		if payload["chat_id"] != "98765" {
			t.Errorf("expected chat_id '98765', got %v", payload["chat_id"])
		}

		if payload["text"] != "<b>[FIRING] Service &lt;Alert&gt;</b>\n\nHigh <b>latency</b> detected on <code>instance-1</code>" {
			t.Errorf("unexpected translated text: %v", payload["text"])
		}

		if payload["parse_mode"] != "HTML" {
			t.Errorf("expected parse_mode 'HTML', got %v", payload["parse_mode"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"result":{"message_id":100}}`))
	}))
	defer mockTelegram.Close()

	// 2. Set up the server
	cfg := &config.Config{Port: 8080}
	rot, _ := proxy.NewRotator([]string{}, 1*time.Second, true)
	s := NewServer(cfg, rot, nil)

	s.rp.Rewrite = func(pr *httputil.ProxyRequest) {
		u, _ := pr.Out.URL.Parse(mockTelegram.URL)
		pr.Out.URL.Scheme = u.Scheme
		pr.Out.URL.Host = u.Host
		pr.Out.Host = u.Host
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/", s.handleCatchAll)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 3. Send a Grafana webhook payload to the proxy
	grafanaPayload := `{
		"receiver": "webhook-point",
		"status": "firing",
		"title": "[FIRING] Service <Alert>",
		"message": "High **latency** detected on ` + "`" + `instance-1` + "`" + `"
	}`
	
	resp, err := http.Post(ts.URL+"/bot12345:token/sendMessage?chat_id=98765", "application/json", strings.NewReader(grafanaPayload))
	if err != nil {
		t.Fatalf("failed to send request to proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(body, &result)

	if result["ok"] != true {
		t.Errorf("expected result ok: true, got %+v", result)
	}
}
