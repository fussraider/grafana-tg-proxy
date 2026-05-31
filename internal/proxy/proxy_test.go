package proxy

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRotatorInitialization(t *testing.T) {
	proxies := []string{
		"socks5://user:pass@1.2.3.4:1080",
		"socks5://[2001:db8::1]:1080",
		"invalid-scheme://5.6.7.8:1080",
	}

	r, err := NewRotator(proxies, 2*time.Second, true)
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}

	if len(r.nodes) != 2 {
		t.Errorf("expected 2 valid nodes parsed, got %d", len(r.nodes))
	}

	node1 := r.nodes[0]
	if node1.Host != "1.2.3.4:1080" {
		t.Errorf("expected node 1 host 1.2.3.4:1080, got %s", node1.Host)
	}
	if node1.Auth == nil || node1.Auth.User != "user" || node1.Auth.Password != "pass" {
		t.Errorf("expected node 1 auth configured, got %+v", node1.Auth)
	}

	node2 := r.nodes[1]
	if node2.Host != "[2001:db8::1]:1080" {
		t.Errorf("expected node 2 host [2001:db8::1]:1080, got %s", node2.Host)
	}
	if node2.Auth != nil {
		t.Errorf("expected node 2 auth nil, got %+v", node2.Auth)
	}
}

func TestRotatorDirectFallback(t *testing.T) {
	// Create a local TCP listener to mock Telegram or endpoint server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	// Spawn acceptor in goroutine
	go func() {
		conn, err := l.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	// Rotator with direct fallback enabled and no proxies
	r, err := NewRotator([]string{}, 2*time.Second, true)
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}

	ctx := context.Background()
	conn, err := r.DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial direct fallback: %v", err)
	}
	defer conn.Close()

	// Rotator with direct fallback disabled and no proxies
	r2, err := NewRotator([]string{}, 2*time.Second, false)
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}

	_, err = r2.DialContext(ctx, "tcp", l.Addr().String())
	if err == nil {
		t.Error("expected error dialing without proxies when direct fallback is disabled, got nil")
	}
}

func TestRotatorProxyBlacklisting(t *testing.T) {
	// Start with mock proxy that fails (non-existent address)
	proxies := []string{
		"socks5://127.0.0.1:54321", // dead proxy
	}

	r, err := NewRotator(proxies, 50*time.Millisecond, true)
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}

	// Direct mock target server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx := context.Background()
	
	// Dial Target. It should fail socks5, mark it unhealthy, then try direct fallback and succeed.
	conn, err := r.DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("expected dial to succeed via direct fallback after proxy failure, got err: %v", err)
	}
	conn.Close()

	// Verify proxy is blacklisted now
	status := r.GetProxiesStatus()
	if status["127.0.0.1:54321"] != false {
		t.Errorf("expected proxy 127.0.0.1:54321 to be unhealthy (false), got %v", status["127.0.0.1:54321"])
	}
}

func TestGetHTTPClient(t *testing.T) {
	r, err := NewRotator([]string{}, 2*time.Second, true)
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}

	client := r.GetHTTPClient()
	if client == nil {
		t.Fatal("expected GetHTTPClient to return non-nil client")
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected client transport to be *http.Transport")
	}

	if transport.DialContext == nil {
		t.Error("expected Transport.DialContext to be configured")
	}
}
