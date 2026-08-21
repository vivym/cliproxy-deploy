package oauthbridge

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	maxRateLimitClients       = 4096
	maxForwardedForHeaderSize = 2048
)

type clientRateLimiter struct {
	mu             sync.Mutex
	limit          int
	window         time.Duration
	trustedProxies []netip.Prefix
	clients        map[string]clientRateLimitEntry
	lastCleanup    time.Time
}

type clientRateLimitEntry struct {
	windowStarted time.Time
	requests      int
}

type fixedWindowRateLimiter struct {
	mu            sync.Mutex
	limit         int
	window        time.Duration
	windowStarted time.Time
	requests      int
}

type rateLimitReservation struct {
	once   sync.Once
	cancel func()
}

func newClientRateLimiter(limit int, window time.Duration, trustedProxies []netip.Prefix) *clientRateLimiter {
	return &clientRateLimiter{
		limit:          limit,
		window:         window,
		trustedProxies: append([]netip.Prefix(nil), trustedProxies...),
		clients:        make(map[string]clientRateLimitEntry),
	}
}

func newFixedWindowRateLimiter(limit int, window time.Duration) *fixedWindowRateLimiter {
	return &fixedWindowRateLimiter{limit: limit, window: window}
}

func (r *rateLimitReservation) Commit() {
	r.once.Do(func() {})
}

func (r *rateLimitReservation) Cancel() {
	r.once.Do(r.cancel)
}

func (l *fixedWindowRateLimiter) Reserve() (*rateLimitReservation, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStarted.IsZero() || now.Before(l.windowStarted) || now.Sub(l.windowStarted) >= l.window {
		l.windowStarted = now
		l.requests = 0
	}
	if l.requests >= l.limit {
		return nil, remainingWindow(now, l.windowStarted, l.window)
	}
	l.requests++
	windowStarted := l.windowStarted
	return &rateLimitReservation{cancel: func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.windowStarted.Equal(windowStarted) && l.requests > 0 {
			l.requests--
		}
	}}, 0
}

func (l *clientRateLimiter) Allow(request *http.Request) (bool, time.Duration) {
	reservation, retryAfter := l.Reserve(request)
	if reservation == nil {
		return false, retryAfter
	}
	reservation.Commit()
	return true, 0
}

func (l *clientRateLimiter) Reserve(request *http.Request) (*rateLimitReservation, time.Duration) {
	now := time.Now()
	client := clientAddress(request, l.trustedProxies)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastCleanup.IsZero() || now.Before(l.lastCleanup) || now.Sub(l.lastCleanup) >= l.window {
		for key, entry := range l.clients {
			if now.Before(entry.windowStarted) || now.Sub(entry.windowStarted) >= l.window {
				delete(l.clients, key)
			}
		}
		l.lastCleanup = now
	}
	entry, exists := l.clients[client]
	if !exists {
		if len(l.clients) >= maxRateLimitClients {
			return nil, l.window
		}
		entry = clientRateLimitEntry{windowStarted: now, requests: 1}
		l.clients[client] = entry
		return l.clientReservation(client, entry.windowStarted), 0
	}
	if now.Before(entry.windowStarted) || now.Sub(entry.windowStarted) >= l.window {
		entry = clientRateLimitEntry{windowStarted: now, requests: 1}
		l.clients[client] = entry
		return l.clientReservation(client, entry.windowStarted), 0
	}
	if entry.requests >= l.limit {
		return nil, remainingWindow(now, entry.windowStarted, l.window)
	}
	entry.requests++
	l.clients[client] = entry
	return l.clientReservation(client, entry.windowStarted), 0
}

func (l *clientRateLimiter) clientReservation(client string, windowStarted time.Time) *rateLimitReservation {
	return &rateLimitReservation{cancel: func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entry, exists := l.clients[client]
		if !exists || !entry.windowStarted.Equal(windowStarted) || entry.requests == 0 {
			return
		}
		entry.requests--
		if entry.requests == 0 {
			delete(l.clients, client)
			return
		}
		l.clients[client] = entry
	}}
}

func remainingWindow(now, started time.Time, window time.Duration) time.Duration {
	remaining := window - now.Sub(started)
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}

func clientAddress(request *http.Request, trustedProxies []netip.Prefix) string {
	peer, ok := parseRequestAddress(request.RemoteAddr)
	if !ok {
		return "unknown"
	}
	peer = peer.Unmap()
	if !addressInPrefixes(peer, trustedProxies) {
		return rateLimitAddressKey(peer)
	}
	forwarded := strings.Join(request.Header.Values("X-Forwarded-For"), ",")
	if forwarded == "" || len(forwarded) > maxForwardedForHeaderSize {
		return rateLimitAddressKey(peer)
	}
	chain := strings.Split(forwarded, ",")
	current := peer
	for index := len(chain) - 1; index >= 0; index-- {
		if !addressInPrefixes(current, trustedProxies) {
			break
		}
		next, err := netip.ParseAddr(strings.TrimSpace(chain[index]))
		if err != nil {
			return rateLimitAddressKey(peer)
		}
		current = next.Unmap()
	}
	return rateLimitAddressKey(current)
}

func rateLimitAddressKey(address netip.Addr) string {
	address = address.Unmap()
	if address.Is6() {
		return netip.PrefixFrom(address, 64).Masked().String()
	}
	return address.String()
}

func parseRequestAddress(raw string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(raw)
	if err == nil {
		address, parseErr := netip.ParseAddr(host)
		return address, parseErr == nil
	}
	address, parseErr := netip.ParseAddr(strings.Trim(raw, "[]"))
	return address, parseErr == nil
}

func addressInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
