package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginRejectsOversizedBody(t *testing.T) {
	old := adminLoginAttempts
	adminLoginAttempts = &loginLimiter{clients: make(map[string]loginAttempt)}
	t.Cleanup(func() { adminLoginAttempts = old })
	rec := httptest.NewRecorder()
	handleAdminLogin(rec, httptest.NewRequest("POST", "/admin/login", strings.NewReader(`{"username":"`+strings.Repeat("a", maxLoginBodyBytes)+`","password":"x"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginLimiterConcurrencyExpiryAndIsolation(t *testing.T) {
	l := &loginLimiter{clients: make(map[string]loginAttempt)}
	now := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("127.0.0.1", now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != maxLoginAttempts {
		t.Fatalf("allowed %d attempts", allowed.Load())
	}
	if !l.allow("127.0.0.2", now) {
		t.Fatal("one client's failures blocked another client")
	}
	if !l.allow("127.0.0.1", now.Add(loginAttemptWindow)) {
		t.Fatal("expired attempts were not pruned")
	}
	l.clear("127.0.0.1")
	if !l.allow("127.0.0.1", now) {
		t.Fatal("successful login should reset limit")
	}
	for i := 0; i < maxLoginClients; i++ {
		l.clients[string(rune(i))] = loginAttempt{expires: now.Add(loginAttemptWindow)}
	}
	if l.allow("new-peer", now) {
		t.Fatal("limiter cache exceeded its bound")
	}
}

func TestLoginRateLimitUsesDirectPeer(t *testing.T) {
	old := adminLoginAttempts
	adminLoginAttempts = &loginLimiter{clients: make(map[string]loginAttempt)}
	t.Cleanup(func() { adminLoginAttempts = old })
	for i := 0; i <= maxLoginAttempts; i++ {
		req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(`{`))
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("X-Forwarded-For", string(rune('a'+i)))
		rec := httptest.NewRecorder()
		handleAdminLogin(rec, req)
		want := 400
		if i == maxLoginAttempts {
			want = 429
		}
		if rec.Code != want {
			t.Fatalf("attempt %d status=%d want=%d", i, rec.Code, want)
		}
		if want == 429 && rec.Header().Get("Retry-After") == "" {
			t.Fatal("rate limit has no retry hint")
		}
	}
}
