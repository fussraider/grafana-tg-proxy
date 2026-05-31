package worker

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestModifyPayloadJSON(t *testing.T) {
	origTime := time.Date(2026, time.May, 31, 16, 5, 0, 0, time.UTC)
	payload := `{"chat_id":"123","text":"High CPU Alert"}`

	modified := modifyPayload(payload, 2, origTime)

	var data map[string]any
	if err := json.Unmarshal([]byte(modified), &data); err != nil {
		t.Fatalf("failed to parse modified JSON payload: %v", err)
	}

	if data["chat_id"] != "123" {
		t.Errorf("expected chat_id 123, got %v", data["chat_id"])
	}

	text, ok := data["text"].(string)
	if !ok {
		t.Fatal("text field is missing or not a string")
	}

	if !strings.Contains(text, "High CPU Alert") {
		t.Errorf("expected text to contain High CPU Alert, got %s", text)
	}

	if !strings.Contains(text, "Retry Attempt #2") {
		t.Errorf("expected text to contain Retry Attempt #2, got %s", text)
	}

	if !strings.Contains(text, "Original Time: 2026-05-31 16:05:00 UTC") {
		t.Errorf("expected text to contain Original Time: 2026-05-31 16:05:00 UTC, got %s", text)
	}
}

func TestModifyPayloadForm(t *testing.T) {
	origTime := time.Date(2026, time.May, 31, 16, 5, 0, 0, time.UTC)
	payload := "chat_id=123&text=Low+Memory+Alert"

	modified := modifyPayload(payload, 3, origTime)

	values, err := url.ParseQuery(modified)
	if err != nil {
		t.Fatalf("failed to parse modified query parameters: %v", err)
	}

	if values.Get("chat_id") != "123" {
		t.Errorf("expected chat_id 123, got %s", values.Get("chat_id"))
	}

	text := values.Get("text")
	if !strings.Contains(text, "Low Memory Alert") {
		t.Errorf("expected text to contain Low Memory Alert, got %s", text)
	}

	if !strings.Contains(text, "Retry Attempt #3") {
		t.Errorf("expected text to contain Retry Attempt #3, got %s", text)
	}

	if !strings.Contains(text, "Original Time: 2026-05-31 16:05:00 UTC") {
		t.Errorf("expected text to contain Original Time: 2026-05-31 16:05:00 UTC, got %s", text)
	}
}
