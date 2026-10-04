package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func seedFailoverAccounts(t *testing.T, count int) *AccountPool {
	t.Helper()
	p := seedReliabilityPool(t)
	previous := cooldowns
	cooldowns = &cooldownStore{records: map[string]cooldownRecord{}}
	t.Cleanup(func() { cooldowns = previous })
	p.Accounts = nil
	for i := 0; i < count; i++ {
		id := string(rune('a' + i))
		p.Accounts = append(p.Accounts, &Account{AccountID: id, Email: id + "@example.invalid",
			Status: "active", RefreshToken: id + "-refresh", AccessToken: "workos:" + id,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	}
	cfg := defaultProxyConfig()
	cfg.Strategy = "fill"
	setProxyConfig(cfg)
	return p
}

func TestAccountFailoverRecoversRejectedAccount(t *testing.T) {
	for _, failure := range []int{401, 403, 429} {
		t.Run(http.StatusText(failure), func(t *testing.T) {
			p := seedFailoverAccounts(t, 2)
			calls := 0
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/auth/refresh" {
					return reliabilityResponse(400, `{"error":"invalid_grant"}`), nil
				}
				calls++
				if r.Header.Get("Authorization") == "Bearer workos:a" {
					return reliabilityResponse(failure, `{"error":"rejected"}`), nil
				}
				return reliabilityResponse(200, `{"choices":[{"message":{"content":"OK"}}]}`), nil
			})
			resp, err := callClineAPI(context.Background(), map[string]any{"model": "vendor/model"}, false)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if calls != 2 || p.Accounts[0].UsageCount != 0 || p.Accounts[1].UsageCount != 1 {
				t.Fatalf("failover or usage count failed: calls=%d accounts=%+v", calls, p.Accounts)
			}
			if failure == 401 && p.Accounts[0].Status != "expired" {
				t.Fatal("invalid credentials were not marked expired")
			}
		})
	}
}

func TestAccountFailoverBeforeCompletionWhenRefreshFails(t *testing.T) {
	p := seedFailoverAccounts(t, 2)
	p.Accounts[0].ExpiresAt = 0
	chatCalls := 0
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			return reliabilityResponse(400, `{"error":"invalid_grant"}`), nil
		}
		chatCalls++
		if r.Header.Get("Authorization") != "Bearer workos:b" {
			t.Fatal("completion sent with broken account")
		}
		return reliabilityResponse(200, `{}`), nil
	})
	resp, err := callClineAPI(context.Background(), map[string]any{"model": "vendor/model"}, false)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if chatCalls != 1 || p.Accounts[0].Status != "expired" {
		t.Fatal("failed token refresh was not skipped")
	}
}

func TestAccountFailoverHasBoundAndDoesNotRepeatAccounts(t *testing.T) {
	for _, strategy := range []string{"round_robin", "fill", "random"} {
		t.Run(strategy, func(t *testing.T) {
			seedFailoverAccounts(t, 5)
			cfg := defaultProxyConfig()
			cfg.Strategy = strategy
			setProxyConfig(cfg)
			seen := make(map[string]bool)
			var session string
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				key := r.Header.Get("Authorization")
				if seen[key] {
					t.Fatal("retried an already rejected account")
				}
				seen[key] = true
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				id, _ := body["session_id"].(string)
				if session != "" && id != session {
					t.Fatal("failover changed the request session")
				}
				session = id
				return reliabilityResponse(429, `{"error":"limited"}`), nil
			})
			_, err := callClineAPI(context.Background(), map[string]any{"model": "vendor/model"}, true)
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.Status != 429 || len(seen) != 3 {
				t.Fatalf("retry budget/status failed: %v attempts=%d", err, len(seen))
			}
		})
	}
}

func TestAccountFailoverDoesNotReplayAmbiguousFailure(t *testing.T) {
	for _, status := range []int{0, 400, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			seedFailoverAccounts(t, 2)
			calls := 0
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, errors.New("connection reset")
				}
				return reliabilityResponse(status, `{"error":"failed"}`), nil
			})
			resp, _ := callClineAPI(context.Background(), map[string]any{"model": "vendor/model"}, true)
			if resp != nil {
				resp.Body.Close()
			}
			if calls != 1 {
				t.Fatalf("completion was replayed %d times", calls)
			}
		})
	}
}
