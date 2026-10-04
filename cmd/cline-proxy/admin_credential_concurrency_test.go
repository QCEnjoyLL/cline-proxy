package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualCredentialUpdateWaitsAndUsesRotatedToken(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/users/me") {
			return reliabilityResponse(200, `{"data":{"email":"test@example.invalid"}}`), nil
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return reliabilityResponse(200, refreshedCredentials), nil
		}
		var body struct {
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken != "new-refresh" {
			t.Errorf("manual update sent stale token: %+v err=%v", body, err)
		}
		return reliabilityResponse(200, `{"data":{"accessToken":"manual-access","refreshToken":"manual-refresh","expiresAt":4102444800000}}`), nil
	})
	autoDone := make(chan error, 1)
	go func() { autoDone <- refreshAccountToken(acc) }()
	<-entered
	manualDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		handleAdminAccountCredentials(w, httptest.NewRequest("POST", "/accounts/credentials", strings.NewReader(`{"accountId":"test-account","refreshToken":"old-refresh"}`)))
		manualDone <- w
	}()
	deadline := time.Now().Add(time.Second)
	active := false
	for time.Now().Before(deadline) {
		poolMu.Lock()
		active = acc.credentialUpdate != nil && acc.credentialUpdate.active
		poolMu.Unlock()
		if active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !active || calls.Load() != 1 {
		t.Error("manual refresh did not wait for automatic refresh")
	}
	unblock()
	if err := <-autoDone; err != nil {
		t.Fatal(err)
	}
	w := <-manualDone
	if w.Code != 200 || acc.RefreshToken != "manual-refresh" || acc.credentialUpdate != nil {
		t.Fatalf("manual renewal failed: HTTP %d %s", w.Code, w.Body.String())
	}
}

func TestCredentialReservationBlocksOtherEditsButAllowsOAuthWaitingRefresh(t *testing.T) {
	p := seedReliabilityPool(t)
	acc, update, err := beginCredentialUpdate(p.Accounts[0].AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer finishCredentialUpdate(acc, update)
	if _, _, err := beginCredentialUpdate(acc.AccountID); !errors.Is(err, errCredentialUpdateBusy) {
		t.Fatalf("concurrent edit accepted: %v", err)
	}
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return reliabilityResponse(200, refreshedCredentials), nil
	})
	// Browser authorization has not reached registration yet, so proxy traffic
	// can still refresh and use the original account.
	if err := refreshAccountToken(acc); err != nil || calls.Load() != 1 {
		t.Fatalf("OAuth reservation blocked normal use: %v", err)
	}
	if err := activateCredentialUpdate(acc, update); err != nil {
		t.Fatal(err)
	}
	autoDone := make(chan error, 1)
	go func() { autoDone <- refreshAccountToken(acc) }()
	select {
	case err := <-autoDone:
		t.Fatalf("automatic refresh ran during credential exchange: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	finishCredentialUpdate(acc, update)
	if err := <-autoDone; err != nil || calls.Load() != 2 {
		t.Fatalf("refresh did not resume: %v", err)
	}
}

func TestOAuthReservationRejectsParallelTokenOrOAuthUpdate(t *testing.T) {
	p := seedReliabilityPool(t)
	acc, update, err := beginCredentialUpdate(p.Accounts[0].AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer finishCredentialUpdate(acc, update)
	for _, handler := range []http.HandlerFunc{handleOAuthStart, handleAdminAccountCredentials} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/update", strings.NewReader(`{"accountId":"test-account","refreshToken":"supplied"}`)))
		if w.Code != http.StatusConflict {
			t.Fatalf("parallel update: HTTP %d %s", w.Code, w.Body.String())
		}
	}
}

func TestOAuthStartupFailureReleasesCredentialReservation(t *testing.T) {
	p := seedReliabilityPool(t)
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("OAuth request has no timeout")
		}
		return reliabilityResponse(503, `{"error":"unavailable"}`), nil
	})
	w := httptest.NewRecorder()
	handleOAuthStart(w, httptest.NewRequest("POST", "/oauth/start", strings.NewReader(`{"accountId":"test-account"}`)))
	if w.Code != 500 || p.Accounts[0].credentialUpdate != nil {
		t.Fatalf("failed startup retained reservation: %s", w.Body.String())
	}
	acc, update, err := beginCredentialUpdate("test-account")
	if err != nil {
		t.Fatal(err)
	}
	finishCredentialUpdate(acc, update)
}

func TestOAuthRegistrationBlocksAutomaticRefreshAndReleasesOnFailure(t *testing.T) {
	p := seedReliabilityPool(t)
	resetOAuthSessions()
	t.Cleanup(resetOAuthSessions)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var refreshCalls atomic.Int32
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("authentication exchange has no timeout")
		}
		switch r.URL.Path {
		case "/user_management/authorize/device":
			return reliabilityResponse(200, `{"device_code":"device","expires_in":300}`), nil
		case "/user_management/authenticate":
			return reliabilityResponse(200, `{"access_token":"access","refresh_token":"refresh"}`), nil
		case "/api/v1/auth/register":
			close(entered)
			<-release
			return reliabilityResponse(503, `{"error":"unavailable"}`), nil
		default:
			refreshCalls.Add(1)
			return reliabilityResponse(200, refreshedCredentials), nil
		}
	})
	w := httptest.NewRecorder()
	handleOAuthStart(w, httptest.NewRequest("POST", "/oauth/start", strings.NewReader(`{"accountId":"test-account"}`)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("OAuth registration was not reached")
	}
	done := make(chan error, 1)
	go func() { done <- refreshAccountToken(p.Accounts[0]) }()
	select {
	case err := <-done:
		t.Fatalf("automatic refresh overlapped registration: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil || refreshCalls.Load() != 1 {
			t.Fatalf("automatic refresh failed to resume: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OAuth failure did not release account")
	}
}

func TestLegacyAccountBindsOfficialEmailOnce(t *testing.T) {
	for _, label := range []string{"user_2", "batch_123", "sso_user_456", "unknown", ""} {
		t.Run(label, func(t *testing.T) {
			p := seedReliabilityPool(t)
			acc := p.Accounts[0]
			acc.Email = label
			next := replacementCredentials{email: "real@example.invalid", accessToken: "a", refreshToken: "r", expiresAt: 4102444800000}
			if err := replaceAccountCredentials(acc.AccountID, next); err != nil {
				t.Fatal(err)
			}
			if acc.Email != next.email || len(p.Accounts) != 1 {
				t.Fatal("legacy record was not migrated in place")
			}
			next.email = "other@example.invalid"
			if err := replaceAccountCredentials(acc.AccountID, next); !errors.Is(err, errCredentialEmailMismatch) {
				t.Fatalf("bound account accepted another identity: %v", err)
			}
		})
	}
}

func TestTokenImportsResolveOfficialEmail(t *testing.T) {
	for _, handler := range []http.HandlerFunc{handleAdminAccountAdd, handleBatchImport, handleSSOImport} {
		p := seedReliabilityPool(t)
		mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/users/me") {
				return reliabilityResponse(200, `{"data":{"email":"real@example.invalid"}}`), nil
			}
			return reliabilityResponse(200, refreshedCredentials), nil
		})
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/import", strings.NewReader(`{"refreshToken":"token","tokens":[{"refreshToken":"token"}],"ssoCookies":"workos:token"}`)))
		if w.Code != 200 || len(p.Accounts) != 2 || p.Accounts[1].Email != "real@example.invalid" {
			t.Fatalf("import email unresolved: HTTP %d %s", w.Code, w.Body.String())
		}
	}
}
