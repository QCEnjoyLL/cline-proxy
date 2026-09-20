package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const maxLoginBodyBytes = 16 << 10
const loginAttemptWindow = 5 * time.Minute
const maxLoginAttempts = 10
const maxLoginClients = 4096

type loginAttempt struct {
	count   int
	expires time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]loginAttempt
}

var adminLoginAttempts = &loginLimiter{clients: make(map[string]loginAttempt)}

// Reserve before checking credentials, so concurrent requests cannot bypass the
// limit. Ignore forwarded headers: only the direct peer is trusted by default.
func (l *loginLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, attempt := range l.clients {
		if !now.Before(attempt.expires) {
			delete(l.clients, key)
		}
	}
	attempt, exists := l.clients[ip]
	if !exists {
		if len(l.clients) >= maxLoginClients {
			return false
		}
		attempt.expires = now.Add(loginAttemptWindow)
	}
	if attempt.count >= maxLoginAttempts {
		return false
	}
	attempt.count++
	l.clients[ip] = attempt
	return true
}

func (l *loginLimiter) clear(ip string) { l.mu.Lock(); delete(l.clients, ip); l.mu.Unlock() }
func remoteLoginIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
