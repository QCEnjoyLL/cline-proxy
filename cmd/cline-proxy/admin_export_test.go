package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminAccountExportMatchesBatchImportFormat(t *testing.T) {
	useTemporaryPool(t)
	pool = &AccountPool{Accounts: []*Account{
		{
			AccountID:    "acc_1",
			Email:        "user@example.com",
			RefreshToken: "refresh-secret",
			AccessToken:  "access-secret",
			Status:       "active",
			CreatedAt:    time.Now(),
		},
	}}

	response := httptest.NewRecorder()
	handleAdminAccountExport(response, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("export status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="cline-accounts.json"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	raw := response.Body.Bytes()
	var exported []accountTransfer
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(exported) != 1 || exported[0].RefreshToken != "refresh-secret" || exported[0].Email != "user@example.com" {
		t.Fatalf("unexpected export: %+v", exported)
	}
	for _, forbidden := range []string{"accountId", "access-secret", "status", "usageCount"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("export leaked %q: %s", forbidden, raw)
		}
	}

	batchRequest, err := json.Marshal(struct {
		Tokens []accountTransfer `json:"tokens"`
	}{Tokens: exported})
	if err != nil {
		t.Fatalf("marshal batch request: %v", err)
	}
	var decoded struct {
		Tokens []accountTransfer `json:"tokens"`
	}
	if err := json.Unmarshal(batchRequest, &decoded); err != nil || len(decoded.Tokens) != 1 {
		t.Fatalf("export is not batch-import compatible: tokens=%+v err=%v", decoded.Tokens, err)
	}
}

func TestAdminAccountExportRequiresLogin(t *testing.T) {
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	response := httptest.NewRecorder()
	requireAdminAuth(mux).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/export", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAdminAccountExportRejectsWrongMethod(t *testing.T) {
	response := httptest.NewRecorder()
	handleAdminAccountExport(response, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/export", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST export status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
