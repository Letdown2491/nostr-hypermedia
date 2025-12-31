package main

import (
	"context"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DNS cache configuration
const (
	dnsCacheTTL     = 5 * time.Minute
	dnsCacheMaxSize = 500
)

// WebSocket dialer timeouts
const (
	wsHandshakeTimeout = 10 * time.Second
	wsDialTimeout      = 10 * time.Second
	wsKeepAlive        = 30 * time.Second
)

type dnsCacheEntry struct {
	ips       []net.IP
	expiresAt time.Time
	safe      bool // cached safety check result
}

var (
	dnsCache   = make(map[string]*dnsCacheEntry)
	dnsCacheMu sync.RWMutex
)

// DNS cache cleanup goroutine
func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			dnsCacheMu.Lock()
			now := time.Now()
			for host, entry := range dnsCache {
				if now.After(entry.expiresAt) {
					delete(dnsCache, host)
				}
			}
			dnsCacheMu.Unlock()
		}
	}()
}

// lookupIPCached performs DNS lookup with caching
func lookupIPCached(host string) ([]net.IP, error) {
	dnsCacheMu.RLock()
	entry, exists := dnsCache[host]
	dnsCacheMu.RUnlock()

	if exists && time.Now().Before(entry.expiresAt) {
		return entry.ips, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	dnsCacheMu.Lock()
	// Evict oldest entries if at max size
	if len(dnsCache) >= dnsCacheMaxSize {
		dnsCacheEvictOldest()
	}
	dnsCache[host] = &dnsCacheEntry{
		ips:       ips,
		expiresAt: time.Now().Add(dnsCacheTTL),
	}
	dnsCacheMu.Unlock()

	return ips, nil
}

// dnsCacheEvictOldest removes 10% of oldest entries (must hold write lock)
func dnsCacheEvictOldest() {
	toRemove := dnsCacheMaxSize / 10
	if toRemove < 1 {
		toRemove = 1
	}

	type hostExpiry struct {
		host      string
		expiresAt time.Time
	}

	entries := make([]hostExpiry, 0, len(dnsCache))
	for host, entry := range dnsCache {
		entries = append(entries, hostExpiry{host, entry.expiresAt})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].expiresAt.Before(entries[j].expiresAt)
	})

	for i := 0; i < toRemove && i < len(entries); i++ {
		delete(dnsCache, entries[i].host)
	}
}

// cachedDialContext creates a DialContext function that uses the DNS cache.
// This speeds up connections by avoiding repeated DNS lookups for the same hosts.
func cachedDialContext(baseDialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			// No port, try dialing as-is
			return baseDialer.DialContext(ctx, network, addr)
		}

		// Check if it's already an IP address
		if ip := net.ParseIP(host); ip != nil {
			return baseDialer.DialContext(ctx, network, addr)
		}

		// Lookup with cache
		ips, err := lookupIPCached(host)
		if err != nil {
			return nil, err
		}

		// Try connecting to each IP
		var lastErr error
		for _, ip := range ips {
			// Prefer IPv4 for reliability
			if ip.To4() != nil {
				conn, err := baseDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
		}

		// Try IPv6 if IPv4 failed
		for _, ip := range ips {
			if ip.To4() == nil {
				conn, err := baseDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
		}

		if lastErr != nil {
			return nil, lastErr
		}

		// Fallback to system resolution
		return baseDialer.DialContext(ctx, network, addr)
	}
}

// baseDialer is the underlying net.Dialer with proper timeouts
var baseDialer = &net.Dialer{
	Timeout:   wsDialTimeout,
	KeepAlive: wsKeepAlive,
}

// wsDialer is the shared WebSocket dialer with DNS caching and proper timeouts.
// Use this for all WebSocket connections in the codebase.
var wsDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: wsHandshakeTimeout,
	NetDialContext:   cachedDialContext(baseDialer),
}
