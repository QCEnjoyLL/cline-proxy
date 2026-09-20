package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountBalanceUsesOfficialUserIDAndCachesZero(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	acc.AccessToken, acc.ExpiresAt = "workos:cached", time.Now().Add(time.Hour).UnixMilli()
	calls := 0
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer workos:cached" {
			t.Fatal("invalid authenticated GET")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		switch r.URL.Path {
		case "/api/v1/users/me":
			return reliabilityResponse(200, `{"success":true,"data":{"id":"user_official"}}`), nil
		case "/api/v1/users/user_official/balance":
			return reliabilityResponse(200, `{"success":true,"data":{"balance":0}}`), nil
		default:
			t.Fatalf("used local account ID or wrong URL: %s", r.URL.Path)
			return nil, nil
		}
	})
	for i := 0; i < 2; i++ {
		got, err := cachedAccountBalance(context.Background(), acc, false)
		if err != nil || got.Balance != 0 || got.CheckedAt == 0 {
			t.Fatalf("balance=%+v error=%v", got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("cache missed: %d calls", calls)
	}
	if _, err := cachedAccountBalance(context.Background(), acc, true); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatal("explicit refresh did not fetch")
	}
	data, _ := json.Marshal(acc)
	if strings.Contains(string(data), "checkedAt") {
		t.Fatal("ephemeral balance leaked into persistent account data")
	}
}

func TestAccountBalanceDoesNotTreatFailuresAsZero(t *testing.T) {
	for _, body := range []string{`<html>error</html>`, `{"success":false,"data":{"balance":0}}`, `{"data":{}}`, `{"data":{"balance":"0.5"}}`, `{"data":null}`} {
		t.Run(body, func(t *testing.T) {
			p := seedReliabilityPool(t)
			acc := p.Accounts[0]
			acc.AccessToken, acc.ExpiresAt = "workos:cached", time.Now().Add(time.Hour).UnixMilli()
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/me") {
					return reliabilityResponse(200, `{"data":{"id":"user"}}`), nil
				}
				return reliabilityResponse(200, body), nil
			})
			got, err := cachedAccountBalance(context.Background(), acc, false)
			if err == nil || got != nil {
				t.Fatalf("invalid response reported as balance: %+v %v", got, err)
			}
		})
	}
}

func TestAccountBalanceRefreshesRejectedTokenOnce(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	acc.AccessToken, acc.ExpiresAt = "workos:old", time.Now().Add(time.Hour).UnixMilli()
	refreshes := 0
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/auth/refresh") {
			refreshes++
			return reliabilityResponse(200, refreshedCredentials), nil
		}
		if r.Header.Get("Authorization") == "Bearer workos:old" {
			return reliabilityResponse(401, `{}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer workos:new-access" {
			t.Fatal("refreshed token not used")
		}
		if strings.HasSuffix(r.URL.Path, "/me") {
			return reliabilityResponse(200, `{"data":{"id":"user"}}`), nil
		}
		return reliabilityResponse(200, `{"data":{"balance":0.5}}`), nil
	})
	got, err := cachedAccountBalance(context.Background(), acc, false)
	if err != nil || got.Balance != 0.5 || refreshes != 1 || acc.RefreshToken != "new-refresh" {
		t.Fatalf("refresh failed: %+v %v", got, err)
	}
}

func TestAccountBalanceConcurrentQueriesShareRequestAndIsolateAccounts(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	acc.AccessToken, acc.ExpiresAt = "workos:first", time.Now().Add(time.Hour).UnixMilli()
	second := &Account{AccountID: "second", AccessToken: "workos:second", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/me") {
			id := "first"
			if r.Header.Get("Authorization") == "Bearer workos:second" {
				id = "second"
			}
			return reliabilityResponse(200, `{"data":{"id":"`+id+`"}}`), nil
		}
		if strings.Contains(r.URL.Path, "/second/") {
			return reliabilityResponse(200, `{"data":{"balance":2}}`), nil
		}
		return reliabilityResponse(200, `{"data":{"balance":1}}`), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cachedAccountBalance(context.Background(), acc, false)
			if err != nil || got.Balance != 1 {
				t.Errorf("shared query: %+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	got, err := cachedAccountBalance(context.Background(), second, false)
	if err != nil || got.Balance != 2 || calls.Load() != 4 {
		t.Fatalf("cache crossed accounts or duplicated requests: %+v %v calls=%d", got, err, calls.Load())
	}
}

func TestAccountBalanceEndpointValidationAndAuth(t *testing.T) {
	seedReliabilityPool(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/admin/api/accounts/balance", 405},
		{"GET", "/admin/api/accounts/balance", 400},
		{"GET", "/admin/api/accounts/balance?accountId=missing", 404},
	} {
		w := httptest.NewRecorder()
		handleAdminAccountBalance(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("status=%d, want %d", w.Code, tc.status)
		}
	}
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	w := httptest.NewRecorder()
	requireAdminAuth(mux).ServeHTTP(w, httptest.NewRequest("GET", "/admin/api/accounts/balance?accountId=test-account", nil))
	if w.Code != 401 {
		t.Fatal("balance endpoint lacks admin authentication")
	}
}
