package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"grafana-tg-proxy/internal/config"
	"grafana-tg-proxy/internal/db"
	"grafana-tg-proxy/internal/logger"
	"grafana-tg-proxy/internal/metrics"
	"grafana-tg-proxy/internal/proxy"
)

// Worker runs the background loop to retry sending failed alerts.
type Worker struct {
	cfg     *config.Config
	database *db.DB
	rotator *proxy.Rotator
	log     *logger.Logger
	stop    chan struct{}
}

// NewWorker initializes a new Worker instance.
func NewWorker(cfg *config.Config, database *db.DB, rot *proxy.Rotator) *Worker {
	return &Worker{
		cfg:      cfg,
		database: database,
		rotator:  rot,
		log:      logger.New("worker"),
		stop:     make(chan struct{}),
	}
}

// Start boots the background loop using the configured retry check interval.
func (w *Worker) Start() {
	ticker := time.NewTicker(w.cfg.RetryCheckInterval)
	w.log.Info("Background retry worker started (interval: %v)", w.cfg.RetryCheckInterval)

	go func() {
		for {
			select {
			case <-ticker.C:
				w.processPendingAlerts()
			case <-w.stop:
				ticker.Stop()
				w.log.Info("Background retry worker stopped")
				return
			}
		}
	}()
}

// Stop shuts down the background loop.
func (w *Worker) Stop() {
	close(w.stop)
}

func (w *Worker) processPendingAlerts() {
	w.updateQueueMetric()
	now := time.Now()
	alerts, err := w.database.GetPendingAlerts(now)
	if err != nil {
		w.log.Error("Failed to query pending alerts: %v", err)
		return
	}

	if len(alerts) > 0 {
		w.log.Info("Found %d pending alerts in spool", len(alerts))
	}

	for _, alert := range alerts {
		// Lock the alert to prevent double delivery
		err := w.database.UpdateAlert(alert.ID, alert.Attempts, alert.NextRetry, "sending")
		if err != nil {
			w.log.Error("Failed to lock alert ID %d: %v", alert.ID, err)
			continue
		}

		go w.retryDeliver(alert)
	}
}

func (w *Worker) retryDeliver(alert *db.Alert) {
	// Increment attempts for the warning message
	currentAttempt := alert.Attempts + 1
	modifiedPayload := modifyPayload(alert.Payload, currentAttempt, alert.OriginalTime)

	// Determine request URL with original query parameters
	reqURL := "https://api.telegram.org/bot" + alert.BotToken + "/sendMessage"
	if alert.QueryParams != "" {
		reqURL = reqURL + "?" + alert.QueryParams
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, strings.NewReader(modifiedPayload))
	if err != nil {
		w.log.Error("Failed to build request for alert ID %d: %v", alert.ID, err)
		w.unlockAlert(alert, currentAttempt, err.Error())
		return
	}

	// Detect payload type to set Content-Type header
	if strings.HasPrefix(strings.TrimSpace(alert.Payload), "{") {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	client := w.rotator.GetHTTPClient()
	
	w.log.Info("Retrying delivery for alert ID %d (Attempt #%d)...", alert.ID, currentAttempt)
	resp, err := client.Do(req)
	if err != nil {
		w.log.Warn("Alert ID %d delivery attempt failed: %v", alert.ID, err)
		w.unlockAlert(alert, currentAttempt, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		w.log.Info("Alert ID %d successfully delivered. Removing from database.", alert.ID)
		if err := w.database.DeleteAlert(alert.ID); err != nil {
			w.log.Error("Failed to delete alert ID %d from database: %v", alert.ID, err)
		}
		route := "socks5"
		if len(w.rotator.GetProxiesStatus()) == 0 {
			route = "direct"
		}
		metrics.IncAlertsDelivered(route, "retry")
		w.updateQueueMetric()
		return
	}

	// Read Telegram API response for logging
	w.log.Warn("Alert ID %d rejected by Telegram: HTTP status %d", alert.ID, resp.StatusCode)
	w.unlockAlert(alert, currentAttempt, fmt.Sprintf("Telegram API HTTP %d", resp.StatusCode))
}

func (w *Worker) unlockAlert(alert *db.Alert, attempts int, reason string) {
	metrics.IncAlertsFailed("retry_failed")

	// Calculate exponential backoff (e.g. 2^attempts * 30 seconds, max 1 hour)
	backoffSec := int(math.Pow(2, float64(attempts))) * 30
	if backoffSec > 3600 {
		backoffSec = 3600 // max 1 hour
	}
	
	nextRetry := time.Now().Add(time.Duration(backoffSec) * time.Second)
	w.log.Info("Rescheduling alert ID %d for retry in %v (at %v)", alert.ID, time.Duration(backoffSec)*time.Second, nextRetry.Format("15:04:05"))

	// Revert status to 'pending' to allow retry worker to pick it up again
	err := w.database.UpdateAlert(alert.ID, attempts, nextRetry, "pending")
	if err != nil {
		w.log.Error("Failed to update status for alert ID %d: %v", alert.ID, err)
	}
	w.updateQueueMetric()
}

func (w *Worker) updateQueueMetric() {
	if count, err := w.database.GetAlertsCount(); err == nil {
		metrics.SetQueuedAlerts(count)
	}
}

// modifyPayload parses JSON or Form URL payloads and appends the warning message with the original timestamp.
func modifyPayload(payload string, attempts int, originalTime time.Time) string {
	warning := fmt.Sprintf("\n\n[⚠️ Delayed Alert | Retry Attempt #%d | Original Time: %s UTC]",
		attempts, originalTime.UTC().Format("2006-01-02 15:04:05"))

	// 1. Try parsing JSON
	var jsonMap map[string]any
	if err := json.Unmarshal([]byte(payload), &jsonMap); err == nil {
		if textVal, ok := jsonMap["text"].(string); ok {
			jsonMap["text"] = textVal + warning
			if updated, err := json.Marshal(jsonMap); err == nil {
				return string(updated)
			}
		} else if capVal, ok := jsonMap["caption"].(string); ok {
			jsonMap["caption"] = capVal + warning
			if updated, err := json.Marshal(jsonMap); err == nil {
				return string(updated)
			}
		}
	}

	// 2. Try parsing URL Form Encoded parameters
	if values, err := url.ParseQuery(payload); err == nil && len(values) > 0 {
		if textVal := values.Get("text"); textVal != "" {
			values.Set("text", textVal+warning)
			return values.Encode()
		} else if capVal := values.Get("caption"); capVal != "" {
			values.Set("caption", capVal+warning)
			return values.Encode()
		}
	}

	// 3. Fallback
	return payload
}
