package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"grafana-tg-proxy/internal/logger"
	"grafana-tg-proxy/internal/metrics"
	"golang.org/x/net/proxy"
)

// ProxyNode represents a single SOCKS5 proxy with its health status.
type ProxyNode struct {
	URL            string
	Host           string
	Auth           *proxy.Auth
	UnhealthyUntil time.Time
}

// Rotator manages SOCKS5 proxy selection, failover, and direct routing.
type Rotator struct {
	mu             sync.RWMutex
	nodes          []*ProxyNode
	currentIndex   int
	dialTimeout    time.Duration
	directFallback bool
	log            *logger.Logger
}

// NewRotator creates a new SOCKS5 proxy rotator instance.
func NewRotator(proxies []string, timeout time.Duration, directFallback bool) (*Rotator, error) {
	log := logger.New("proxy")
	var nodes []*ProxyNode

	for _, p := range proxies {
		u, err := url.Parse(p)
		if err != nil {
			log.Error("Failed to parse SOCKS5 proxy URL %s: %v", p, err)
			continue
		}

		if u.Scheme != "socks5" {
			log.Warn("Skipping SOCKS5 proxy %s: unsupported scheme %q (must be socks5)", p, u.Scheme)
			continue
		}

		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{
				User:     u.User.Username(),
				Password: password,
			}
		}

		nodes = append(nodes, &ProxyNode{
			URL:  p,
			Host: u.Host,
			Auth: auth,
		})
	}

	// Initialize proxy health status metrics
	for _, node := range nodes {
		metrics.SetProxyHealth(node.Host, true)
	}

	return &Rotator{
		nodes:          nodes,
		dialTimeout:    timeout,
		directFallback: directFallback,
		log:            log,
	}, nil
}

// TestProxies performs a startup connectivity test for all configured SOCKS5 proxies.
// It logs the status and latency (ping) for each proxy, and warns if any are offline.
func (r *Rotator) TestProxies(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.nodes) == 0 {
		return
	}

	r.log.Info("Running startup connectivity test for %d SOCKS5 proxies...", len(r.nodes))

	for i, node := range r.nodes {
		maskedURL := logger.Redact(node.URL)

		start := time.Now()
		conn, err := r.dialSOCKS5(ctx, node, "tcp", "api.telegram.org:443")
		duration := time.Since(start)

		if err == nil {
			conn.Close()
			r.log.Info("  Proxy [%d]: %s - ONLINE, ping: %v", i+1, maskedURL, duration.Round(time.Millisecond))
			metrics.SetProxyHealth(node.Host, true)
		} else {
			node.UnhealthyUntil = time.Now().Add(60 * time.Second)
			metrics.SetProxyHealth(node.Host, false)
			r.log.Warn("  Proxy [%d]: %s - OFFLINE (error: %v)", i+1, maskedURL, err)
		}
	}
}

// GetProxiesStatus returns health status of all proxies in the pool.
func (r *Rotator) GetProxiesStatus() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	status := make(map[string]bool)
	now := time.Now()
	for _, node := range r.nodes {
		status[node.Host] = now.After(node.UnhealthyUntil)
	}
	return status
}

// DialContext establishes a connection to the address, routing it through the SOCKS5 pool.
// It cycles through proxies on failure and falls back to a direct connection if necessary.
func (r *Rotator) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	totalNodes := len(r.nodes)
	if totalNodes == 0 {
		if r.directFallback {
			r.log.Warn("No SOCKS5 proxies configured. Dialing direct to %s", addr)
			return r.dialDirect(ctx, network, addr)
		}
		return nil, fmt.Errorf("no proxies configured and direct fallback is disabled")
	}

	now := time.Now()
	var lastErr error

	// Try all nodes in the rotator starting from the current index
	for i := 0; i < totalNodes; i++ {
		nodeIdx := (r.currentIndex + i) % totalNodes
		node := r.nodes[nodeIdx]

		// Skip unhealthy nodes
		if now.Before(node.UnhealthyUntil) {
			r.log.Debug("Skipping unhealthy proxy %s (cooldown active)", node.Host)
			continue
		}

		// Try to connect through this proxy
		r.log.Info("Attempting to route connection to %s via SOCKS5 proxy [%s]", addr, node.Host)
		conn, err := r.dialSOCKS5(ctx, node, network, addr)
		if err == nil {
			// Success! Update current index for the next call
			r.currentIndex = (nodeIdx + 1) % totalNodes
			metrics.SetProxyHealth(node.Host, true)
			return conn, nil
		}

		// Mark unhealthy on connection failure
		node.UnhealthyUntil = time.Now().Add(60 * time.Second)
		metrics.SetProxyHealth(node.Host, false)
		r.log.Warn("Connection failed via SOCKS5 proxy [%s]: %v. Marked unhealthy for 60s", node.Host, err)
		lastErr = err
	}

	// If all SOCKS5 proxies failed, try direct fallback
	if r.directFallback {
		r.log.Warn("All SOCKS5 proxies failed. Attempting direct fallback connection to %s", addr)
		return r.dialDirect(ctx, network, addr)
	}

	return nil, fmt.Errorf("all SOCKS5 proxies failed: %w", lastErr)
}

func (r *Rotator) dialSOCKS5(ctx context.Context, node *ProxyNode, network, addr string) (net.Conn, error) {
	baseDialer := &net.Dialer{
		Timeout: r.dialTimeout,
	}

	// Create SOCKS5 dialer
	dialer, err := proxy.SOCKS5("tcp", node.Host, node.Auth, baseDialer)
	if err != nil {
		return nil, err
	}

	// Verify ContextDialer support
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks5 dialer does not support ContextDialer")
	}

	// Ensure dialing obeys context timeout
	return ctxDialer.DialContext(ctx, network, addr)
}

func (r *Rotator) dialDirect(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: r.dialTimeout,
	}
	return dialer.DialContext(ctx, network, addr)
}

// GetHTTPClient returns an HTTP client that routes connection queries through the rotator.
func (r *Rotator) GetHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           r.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
}
