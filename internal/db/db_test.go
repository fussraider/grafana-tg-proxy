package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"grafana-tg-proxy/internal/logger"
)

func TestDatabaseLifecycle(t *testing.T) {
	// Initialize logger configuration for testing
	logger.Setup("debug", "plain", false)

	// Create a temporary directory for the test database
	tempDir, err := os.MkdirTemp("", "db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test_alerts.db")

	var database *DB
	var alertID int

	token := "12345:token-abc"
	chatID := "999888"
	payload := `{"chat_id":"999888","text":"alert firing"}`
	queryParams := "parse_mode=markdown"
	origTime := time.Now().Add(-5 * time.Minute)

	t.Run("InitializeSchema", func(t *testing.T) {
		database = testInitializeSchema(t, dbPath)
	})
	
	// Graceful close after initialization sub-test runs
	defer func() {
		if database != nil {
			database.Close()
		}
	}()

	t.Run("SaveAlert", func(t *testing.T) {
		testSaveAlert(t, database, token, chatID, payload, queryParams, origTime)
	})

	t.Run("GetPendingAlerts", func(t *testing.T) {
		alertID = testGetPendingAlerts(t, database, token, chatID, payload, queryParams)
	})

	t.Run("UpdateAlert", func(t *testing.T) {
		testUpdateAlert(t, database, alertID)
	})

	t.Run("DeleteAlert", func(t *testing.T) {
		testDeleteAlert(t, database, alertID)
	})
}

func testInitializeSchema(t *testing.T, dbPath string) *DB {
	database, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("expected SQLite database file to exist, but it was not found")
	}
	return database
}

func testSaveAlert(t *testing.T, database *DB, token, chatID, payload, queryParams string, origTime time.Time) {
	err := database.SaveAlert(token, chatID, payload, queryParams, origTime)
	if err != nil {
		t.Fatalf("failed to save alert: %v", err)
	}
}

func testGetPendingAlerts(t *testing.T, database *DB, token, chatID, payload, queryParams string) int {
	futureTime := time.Now().Add(30 * time.Second)
	alerts, err := database.GetPendingAlerts(futureTime)
	if err != nil {
		t.Fatalf("failed to query pending alerts: %v", err)
	}

	if len(alerts) != 1 {
		t.Fatalf("expected 1 pending alert, got %d", len(alerts))
	}

	a := alerts[0]
	if a.BotToken != token || a.ChatID != chatID || a.Payload != payload || a.QueryParams != queryParams {
		t.Errorf("spooled alert values mismatch, got: %+v", a)
	}
	return a.ID
}

func testUpdateAlert(t *testing.T, database *DB, alertID int) {
	nextRetry := time.Now().Add(1 * time.Minute)
	err := database.UpdateAlert(alertID, 1, nextRetry, "sending")
	if err != nil {
		t.Fatalf("failed to update alert: %v", err)
	}

	pendingAlerts, err := database.GetPendingAlerts(time.Now().Add(5 * time.Minute))
	if err != nil {
		t.Fatalf("failed to query alerts: %v", err)
	}
	if len(pendingAlerts) != 0 {
		t.Errorf("expected 0 pending alerts after locking status, got %d", len(pendingAlerts))
	}
}

func testDeleteAlert(t *testing.T, database *DB, alertID int) {
	err := database.DeleteAlert(alertID)
	if err != nil {
		t.Fatalf("failed to delete alert: %v", err)
	}

	var count int
	err = database.db.QueryRow("SELECT COUNT(*) FROM pending_alerts").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query database row count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 database records after delete, got %d", count)
	}
}
