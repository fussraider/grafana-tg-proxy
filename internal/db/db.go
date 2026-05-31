package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"grafana-tg-proxy/internal/logger"
	_ "github.com/glebarez/go-sqlite"
)

// Alert represents a spooled message stored in the database.
type Alert struct {
	ID           int
	BotToken     string
	ChatID       string
	Payload      string
	QueryParams  string
	OriginalTime time.Time
	Attempts     int
	NextRetry    time.Time
	Status       string
}

// DB wraps the sql.DB instance and provides helper query methods.
type DB struct {
	db  *sql.DB
	log *logger.Logger
}

// NewDB initializes the SQLite database at dbPath and creates the schema.
func NewDB(dbPath string) (*DB, error) {
	log := logger.New("db")

	// Ensure target directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	// Open connection using glebarez pure Go driver
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Restrict connection limits to prevent writer locks
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Set journal and timeout parameters
	_, err = db.Exec("PRAGMA journal_mode=WAL;")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable WAL: %w", err)
	}

	_, err = db.Exec("PRAGMA busy_timeout=5000;")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set busy timeout: %w", err)
	}

	s := &DB{
		db:  db,
		log: log,
	}

	// Run auto-migrations
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (d *DB) migrate() error {
	var currentVersion int
	err := d.db.QueryRow("PRAGMA user_version;").Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("failed to read schema version: %w", err)
	}

	if currentVersion == 0 {
		d.log.Info("Initializing database schema (Version 1)...")
		
		schemaQuery := `
		CREATE TABLE IF NOT EXISTS pending_alerts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			bot_token TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			query_params TEXT,
			original_time TEXT NOT NULL,
			attempts INTEGER DEFAULT 0,
			next_retry TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending'
		);
		`
		
		tx, err := d.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if _, err := tx.Exec(schemaQuery); err != nil {
			return fmt.Errorf("failed to create tables: %w", err)
		}

		if _, err := tx.Exec("PRAGMA user_version = 1;"); err != nil {
			return fmt.Errorf("failed to set user_version: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration: %w", err)
		}
		
		d.log.Info("Database schema initialized successfully.")
	}

	return nil
}

// SaveAlert serializes a failed alert payload to database spool.
func (d *DB) SaveAlert(token, chatID, payload, queryParams string, originalTime time.Time) error {
	d.log.Info("Saving failed alert to SQLite database (chat_id: %s)...", chatID)
	
	query := `
	INSERT INTO pending_alerts (bot_token, chat_id, payload, query_params, original_time, next_retry, status)
	VALUES (?, ?, ?, ?, ?, ?, 'pending');
	`
	
	// Try initially sending after 10 seconds backoff
	nextRetry := time.Now().Add(10 * time.Second)

	_, err := d.db.Exec(query, token, chatID, payload, queryParams, originalTime.UTC().Format(time.RFC3339), nextRetry.UTC().Format(time.RFC3339))
	if err != nil {
		d.log.Error("Failed to spool alert: %v", err)
		return err
	}
	
	d.log.Debug("Alert spooled successfully.")
	return nil
}

// GetPendingAlerts reads alerts matching retry criteria.
func (d *DB) GetPendingAlerts(now time.Time) ([]*Alert, error) {
	query := "SELECT id, bot_token, chat_id, payload, query_params, original_time, attempts, next_retry, status FROM pending_alerts WHERE next_retry <= ? AND status = 'pending' LIMIT 50"
	
	rows, err := d.db.Query(query, now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []*Alert
	for rows.Next() {
		a := &Alert{}
		var origTime, retryTime string
		
		err := rows.Scan(&a.ID, &a.BotToken, &a.ChatID, &a.Payload, &a.QueryParams, &origTime, &a.Attempts, &retryTime, &a.Status)
		if err != nil {
			d.log.Error("Scan error: %v", err)
			return nil, err
		}

		a.OriginalTime, _ = time.Parse(time.RFC3339, origTime)
		if a.OriginalTime.IsZero() {
			// Fallback parsing for other SQLite timestamp formats
			a.OriginalTime, _ = time.Parse("2006-01-02 15:04:05", origTime)
		}

		a.NextRetry, _ = time.Parse(time.RFC3339, retryTime)
		if a.NextRetry.IsZero() {
			a.NextRetry, _ = time.Parse("2006-01-02 15:04:05", retryTime)
		}

		alerts = append(alerts, a)
	}

	if err = rows.Err(); err != nil {
		d.log.Error("DB DEBUG: rows.Err() after loop: %v", err)
		return nil, err
	}

	return alerts, nil
}

// UpdateAlert updates attempt metrics and locks/unlocks status.
func (d *DB) UpdateAlert(id int, attempts int, nextRetry time.Time, status string) error {
	query := `
	UPDATE pending_alerts
	SET attempts = ?, next_retry = ?, status = ?
	WHERE id = ?;
	`
	_, err := d.db.Exec(query, attempts, nextRetry.UTC().Format(time.RFC3339), status, id)
	return err
}

// DeleteAlert removes an alert from the spool on successful delivery.
func (d *DB) DeleteAlert(id int) error {
	query := `DELETE FROM pending_alerts WHERE id = ?;`
	_, err := d.db.Exec(query, id)
	return err
}

// GetAlertsCount returns the total number of spooled alerts waiting in the database.
func (d *DB) GetAlertsCount() (int, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM pending_alerts").Scan(&count)
	return count, err
}

// Close disconnects SQL database.
func (d *DB) Close() error {
	return d.db.Close()
}
