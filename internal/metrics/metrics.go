package metrics

import (
	"fmt"
	"net/http"

	"grafana-tg-proxy/internal/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	alertsReceived = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "tg_proxy_alerts_received_total",
			Help: "Total number of alerts received from Grafana.",
		},
	)

	alertsDelivered = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tg_proxy_alerts_delivered_total",
			Help: "Total number of alerts successfully sent to Telegram Bot API.",
		},
		[]string{"route", "type"}, // route: "socks5", "direct"; type: "initial", "retry"
	)

	alertsFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tg_proxy_alerts_failed_total",
			Help: "Total count of failed alert delivery attempts.",
		},
		[]string{"reason"}, // e.g. "proxy_error", "timeout", "api_error"
	)

	proxyHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tg_proxy_proxy_health_status",
			Help: "Health status of the SOCKS5 proxy: 1 = healthy, 0 = unhealthy.",
		},
		[]string{"proxy"},
	)

	queuedAlerts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "tg_proxy_queued_alerts_count",
			Help: "Number of alerts currently stored in the SQLite database spool.",
		},
	)
)

func init() {
	// Register metrics with Prometheus default registry
	prometheus.MustRegister(alertsReceived)
	prometheus.MustRegister(alertsDelivered)
	prometheus.MustRegister(alertsFailed)
	prometheus.MustRegister(proxyHealth)
	prometheus.MustRegister(queuedAlerts)
}

// IncAlertsReceived increments the received alerts counter.
func IncAlertsReceived() {
	alertsReceived.Inc()
}

// IncAlertsDelivered increments the delivered alerts counter with labels.
func IncAlertsDelivered(route, msgType string) {
	alertsDelivered.WithLabelValues(route, msgType).Inc()
}

// IncAlertsFailed increments the failed alerts counter with reason label.
func IncAlertsFailed(reason string) {
	alertsFailed.WithLabelValues(reason).Inc()
}

// SetProxyHealth sets the health state gauge for a SOCKS5 proxy address.
func SetProxyHealth(proxyAddr string, healthy bool) {
	val := 1.0
	if !healthy {
		val = 0.0
	}
	proxyHealth.WithLabelValues(proxyAddr).Set(val)
}

// SetQueuedAlerts sets the queue count gauge for spooled database alerts.
func SetQueuedAlerts(count int) {
	queuedAlerts.Set(float64(count))
}

// StartMetricsServer starts a dedicated HTTP listener for Prometheus scraping.
func StartMetricsServer(port int) {
	log := logger.New("metrics")
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	addr := fmt.Sprintf(":%d", port)
	log.Info("Prometheus metrics server starting on http://localhost%s/metrics", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error("Metrics server stopped with error: %v", err)
		}
	}()
}
