package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReplaceAccountCredentialsPreservesAccount(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	acc.Disabled = true
	acc.Status = "expired"
	acc.UsageCount, acc.DailyUsageCount = 45, 3
	acc.CreatedAt = time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	acc.credits = &accountCreditState{}
	created := acc.CreatedAt
	if err := replaceAccountCredentials(acc.AccountID, replacementCredentials{
		email: "TEST@example.invalid", accessToken: "fresh-access",
		refreshToken: "fresh-refresh", expiresAt: 4102444800000,
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts) != 1 || p.Accounts[0] != acc || acc.AccountID != "test-account" || acc.Email != "test@example.invalid" ||
		acc.UsageCount != 45 || acc.DailyUsageCount != 3 || acc.CreatedAt != created || !acc.Disabled ||
		acc.Status != "active" || acc.AccessToken != "workos:fresh-access" || acc.RefreshToken != "fresh-refresh" || acc.credits != nil {
		t.Fatalf("account fields after replacement: %+v", acc)
	}
	poolMu.Lock()
	pool = nil
	poolMu.Unlock()
	reloaded := loadPool()
	if len(reloaded.Accounts) != 1 || reloaded.Accounts[0].RefreshToken != "fresh-refresh" ||
		reloaded.Accounts[0].UsageCount != 45 || !reloaded.Accounts[0].Disabled {
		t.Fatalf("updated credential or account settings not persisted: %+v", reloaded.Accounts)
	}
}

func TestReplaceAccountCredentialsRejectsWrongTarget(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	for _, tc := range []struct {
		id, email string
		want      error
	}{
		{"missing", acc.Email, errCredentialAccountMissing},
		{acc.AccountID, "other@example.invalid", errCredentialEmailMismatch},
	} {
		err := replaceAccountCredentials(tc.id, replacementCredentials{
			email: tc.email, accessToken: "new", refreshToken: "new", expiresAt: 4102444800000,
		})
		if !errors.Is(err, tc.want) || acc.RefreshToken != "old-refresh" || acc.AccessToken != "" {
			t.Fatalf("target=%q: err=%v account=%+v", tc.id, err, acc)
		}
	}
}

func TestManualCredentialUpdateVerifiesOfficialEmail(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		wantCode    int
	}{
		{"same account", "test@example.invalid", http.StatusOK},
		{"different account", "other@example.invalid", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := seedReliabilityPool(t)
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/v1/auth/refresh":
					if r.Method != http.MethodPost {
						t.Fatalf("refresh method = %s", r.Method)
					}
					return reliabilityResponse(200, refreshedCredentials), nil
				case "/api/v1/users/me":
					if r.Header.Get("Authorization") != "Bearer workos:new-access" {
						t.Fatal("identity request lacks new access token")
					}
					return reliabilityResponse(200, `{"success":true,"data":{"email":"`+tc.email+`"}}`), nil
				default:
					t.Fatalf("unexpected endpoint: %s", r.URL.Path)
					return nil, nil
				}
			})
			rec := httptest.NewRecorder()
			handleAdminAccountCredentials(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/credentials", strings.NewReader(`{"accountId":"test-account","refreshToken":"supplied"}`)))
			if rec.Code != tc.wantCode {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
			}
			wantToken := "old-refresh"
			if tc.wantCode == http.StatusOK {
				wantToken = "new-refresh"
			}
			if len(p.Accounts) != 1 || p.Accounts[0].RefreshToken != wantToken {
				t.Fatalf("unexpected account mutation: %+v", p.Accounts)
			}
			if tc.wantCode == http.StatusConflict {
				var body struct {
					Data struct {
						RefreshToken string `json:"refreshToken"`
					} `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.RefreshToken != "new-refresh" {
					t.Fatalf("rotated token was not returned for recovery: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestManualCredentialUpdateReturnsRotatedTokenWhenIdentityUnavailable(t *testing.T) {
	p := seedReliabilityPool(t)
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			return reliabilityResponse(200, refreshedCredentials), nil
		}
		return reliabilityResponse(http.StatusBadGateway, `<html>temporary</html>`), nil
	})
	rec := httptest.NewRecorder()
	handleAdminAccountCredentials(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/credentials", strings.NewReader(`{"accountId":"test-account","refreshToken":"supplied"}`)))
	var body struct {
		Data struct {
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusFailedDependency ||
		body.Data.RefreshToken != "new-refresh" || p.Accounts[0].RefreshToken != "old-refresh" {
		t.Fatalf("rotated token recovery or original account failed: HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestOAuthUpdateStartRequiresExistingAccount(t *testing.T) {
	seedReliabilityPool(t)
	rec := httptest.NewRecorder()
	handleOAuthStart(rec, httptest.NewRequest(http.MethodPost, "/admin/api/oauth/start", strings.NewReader(`{"accountId":"missing"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOAuthStartUpdatesExistingAccountOrImportsNormally(t *testing.T) {
	for _, tc := range []struct {
		name, body, email string
		wantUpdated       bool
		wantSuccess       bool
	}{
		{"update", `{"accountId":"test-account"}`, "test@example.invalid", true, true},
		{"wrong email", `{"accountId":"test-account"}`, "other@example.invalid", false, false},
		{"ordinary import", "", "other@example.invalid", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := seedReliabilityPool(t)
			resetOAuthSessions()
			t.Cleanup(resetOAuthSessions)
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/user_management/authorize/device":
					return reliabilityResponse(200, `{"device_code":"device","user_code":"code","verification_uri":"https://login.example.invalid","expires_in":300}`), nil
				case "/user_management/authenticate":
					return reliabilityResponse(200, `{"access_token":"workos-access","refresh_token":"workos-refresh"}`), nil
				case "/api/v1/auth/register":
					return reliabilityResponse(200, `{"data":{"accessToken":"oauth-access","refreshToken":"oauth-refresh","expiresAt":4102444800000,"userInfo":{"email":"`+tc.email+`"}}}`), nil
				default:
					t.Fatalf("unexpected OAuth endpoint: %s", r.URL.Path)
					return nil, nil
				}
			})
			rec := httptest.NewRecorder()
			handleOAuthStart(rec, httptest.NewRequest(http.MethodPost, "/admin/api/oauth/start", strings.NewReader(tc.body)))
			var start struct {
				Data struct {
					SessionID string `json:"sessionId"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || rec.Code != http.StatusOK || start.Data.SessionID == "" {
				t.Fatalf("start OAuth: HTTP %d %s", rec.Code, rec.Body.String())
			}
			var status struct {
				Data struct {
					Done    bool `json:"done"`
					Success bool `json:"success"`
					Updated bool `json:"updated"`
				} `json:"data"`
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				statusRec := httptest.NewRecorder()
				handleOAuthStatus(statusRec, httptest.NewRequest(http.MethodGet, "/admin/api/oauth/status?sessionId="+start.Data.SessionID, nil))
				if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if status.Data.Done {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !status.Data.Done || status.Data.Success != tc.wantSuccess || status.Data.Updated != tc.wantUpdated {
				t.Fatalf("OAuth status: %+v", status)
			}
			wantAccounts, wantToken := 1, "old-refresh"
			if tc.wantUpdated {
				wantToken = "oauth-refresh"
			}
			if tc.body == "" {
				wantAccounts = 2
			}
			if len(p.Accounts) != wantAccounts || p.Accounts[0].RefreshToken != wantToken {
				t.Fatalf("OAuth affected wrong account: %+v", p.Accounts)
			}
		})
	}
}

func TestOAuthCredentialUpdateChecksIdentity(t *testing.T) {
	p := seedReliabilityPool(t)
	cline := &clineAuthResp{}
	cline.Data.AccessToken = "oauth-access"
	cline.Data.RefreshToken = "oauth-refresh"
	cline.Data.ExpiresAt = int64(4102444800000)
	cline.Data.UserInfo = &struct {
		Email string `json:"email"`
	}{Email: "other@example.invalid"}
	if _, err := replaceOAuthAccountCredentials("test-account", cline); !errors.Is(err, errCredentialEmailMismatch) {
		t.Fatalf("wrong OAuth identity accepted: %v", err)
	}
	if p.Accounts[0].RefreshToken != "old-refresh" {
		t.Fatal("wrong OAuth identity changed account")
	}
	cline.Data.UserInfo.Email = "test@example.invalid"
	if _, err := replaceOAuthAccountCredentials("test-account", cline); err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts) != 1 || p.Accounts[0].RefreshToken != "oauth-refresh" {
		t.Fatal("OAuth update did not replace original credentials")
	}
}

func TestCredentialUpdateWaitsForCurrentRefresh(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	pending := &accountRefresh{done: make(chan struct{})}
	poolMu.Lock()
	acc.refresh = pending
	poolMu.Unlock()
	finished := make(chan error, 1)
	go func() {
		finished <- replaceAccountCredentials(acc.AccountID, replacementCredentials{
			email: acc.Email, accessToken: "replacement", refreshToken: "replacement", expiresAt: 4102444800000,
		})
	}()
	select {
	case err := <-finished:
		t.Fatalf("replacement overtook in-flight refresh: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	poolMu.Lock()
	acc.RefreshToken = "older-rotation"
	acc.refresh = nil
	close(pending.done)
	poolMu.Unlock()
	select {
	case err := <-finished:
		if err != nil || acc.RefreshToken != "replacement" {
			t.Fatalf("replacement lost: %v %+v", err, acc)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement remained blocked")
	}
}
