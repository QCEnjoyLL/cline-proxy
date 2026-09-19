package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func updateConfigForTest(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleAdminUpdateConfig(rec, httptest.NewRequest(http.MethodPost, "/admin/api/config/update", strings.NewReader(body)))
	return rec
}

func TestProxyConfigSurvivesReload(t *testing.T) {
	useTemporaryPool(t)
	body := `{"strategy":"fill","headers":{"X-CLIENT-TYPE":"custom-client","X-Test":"saved"},"defaultModel":"` + defaultModels[1].ID + `","cooldownMinutes":12}`
	if rec := updateConfigForTest(body); rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 后续的增量更新保留先前配置，再通过真实读盘模拟重启。
	if rec := updateConfigForTest(`{"headers":{"X-Second":"also-saved"}}`); rec.Code != http.StatusOK {
		t.Fatalf("partial update status=%d body=%s", rec.Code, rec.Body.String())
	}
	pool = nil
	setProxyConfig(defaultProxyConfig())
	loadPool()
	cfg := getProxyConfig()
	if cfg.Strategy != "fill" || cfg.Headers["X-CLIENT-TYPE"] != "custom-client" || cfg.Headers["X-Test"] != "saved" || cfg.Headers["X-Second"] != "also-saved" {
		t.Fatalf("config after reload: %+v", cfg)
	}
	if cfg.Headers["User-Agent"] != defaultProxyConfig().Headers["User-Agent"] {
		t.Fatal("partial headers update lost built-in headers")
	}
	if getDefaultModel() != defaultModels[1].ID || cooldownMinutes() != 12 {
		t.Fatal("model/cooldown settings did not survive reload")
	}
}

func TestProxyConfigLoadsLegacyPool(t *testing.T) {
	useTemporaryPool(t)
	if err := os.WriteFile(poolPath, []byte(`{"accounts":[{"accountId":"legacy","status":"active"}],"keys":["existing-key"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	p := loadPool()
	if !reflect.DeepEqual(getProxyConfig(), defaultProxyConfig()) {
		t.Fatalf("legacy pool should use defaults: %+v", getProxyConfig())
	}
	if len(p.Accounts) != 1 || p.Accounts[0].AccountID != "legacy" || len(p.Keys) != 1 || p.Keys[0] != "existing-key" {
		t.Fatal("legacy accounts/keys changed")
	}
}

func TestProxyConfigRejectedUpdatePreservesAllSettings(t *testing.T) {
	for _, failure := range []string{"invalid-model", "write-failure"} {
		t.Run(failure, func(t *testing.T) {
			useTemporaryPool(t)
			resetCooldowns()
			t.Cleanup(resetCooldowns)
			if rec := updateConfigForTest(`{"strategy":"fill","headers":{"X-Test":"original"},"cooldownMinutes":12}`); rec.Code != http.StatusOK {
				t.Fatalf("initial update: %s", rec.Body.String())
			}
			cooldowns.mark("acc", "test@example.com", "model", time.Now(), time.Hour, limitInfo{})
			originalConfig := getProxyConfig()
			originalDefault := getDefaultModel()
			originalPath := poolPath
			originalBytes, err := os.ReadFile(originalPath)
			if err != nil {
				t.Fatal(err)
			}
			model := defaultModels[1].ID
			wantStatus := http.StatusInternalServerError
			if failure == "invalid-model" {
				model = "missing/model"
				wantStatus = http.StatusBadRequest
			} else {
				poolPath = filepath.Join(t.TempDir(), "missing-directory", "pool.json")
			}
			rec := updateConfigForTest(`{"strategy":"random","headers":{"X-Test":"changed"},"defaultModel":"` + model + `","cooldownMinutes":5}`)
			if rec.Code != wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
			}
			if !reflect.DeepEqual(getProxyConfig(), originalConfig) || !reflect.DeepEqual(loadPool().ProxyConfig, originalConfig) || getDefaultModel() != originalDefault || cooldownMinutes() != 12 {
				t.Fatal("rejected update changed in-memory settings")
			}
			if !cooldowns.isCooling("acc", "model", time.Now()) {
				t.Fatal("rejected update cleared cooldowns")
			}
			poolPath = originalPath
			afterBytes, err := os.ReadFile(originalPath)
			if err != nil || string(afterBytes) != string(originalBytes) {
				t.Fatalf("rejected update changed saved settings: %v", err)
			}
		})
	}
}

func TestProxyConfigConcurrentUpdatesPreserveHeaders(t *testing.T) {
	useTemporaryPool(t)
	loadPool()
	var wg sync.WaitGroup
	for _, header := range []string{"X-First", "X-Second"} {
		wg.Go(func() {
			rec := updateConfigForTest(`{"headers":{"` + header + `":"saved"}}`)
			if rec.Code != http.StatusOK {
				t.Errorf("update %s status=%d body=%s", header, rec.Code, rec.Body.String())
			}
		})
	}
	wg.Wait()
	pool = nil
	loadPool()
	cfg := getProxyConfig()
	if cfg.Headers["X-First"] != "saved" || cfg.Headers["X-Second"] != "saved" {
		t.Fatalf("concurrent updates lost headers: %v", cfg.Headers)
	}
}

func TestOlderPoolSnapshotCannotOverwriteConfig(t *testing.T) {
	useTemporaryPool(t)
	loadPool()
	// 模拟请求已经取到旧快照，但管理员的配置更新先完成了落盘。
	poolMu.Lock()
	older, err := marshalPool()
	poolMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if rec := updateConfigForTest(`{"strategy":"fill","headers":{"X-Test":"new"}}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %s", rec.Body.String())
	}
	if err := writePoolSnapshot(older); err != nil {
		t.Fatal(err)
	}
	pool = nil
	loadPool()
	if cfg := getProxyConfig(); cfg.Strategy != "fill" || cfg.Headers["X-Test"] != "new" {
		t.Fatalf("stale snapshot overwrote config: %+v", cfg)
	}
}

func TestHealthTracksAccountChanges(t *testing.T) {
	useTemporaryPool(t)
	check := func(want int) {
		t.Helper()
		for _, path := range []string{"/health", "/v1/health"} {
			rec := httptest.NewRecorder()
			handleHealth(rec, httptest.NewRequest(http.MethodGet, path, nil))
			var body struct {
				Status         string `json:"status"`
				Version        string `json:"version"`
				ActiveAccounts int    `json:"activeAccounts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || body.Status != "ok" || body.Version != versionLabel() || body.ActiveAccounts != want {
				t.Fatalf("%s response=%+v status=%d want active=%d", path, body, rec.Code, want)
			}
		}
	}
	check(0)
	acc := &Account{AccountID: "added-later", Status: "active"}
	addAccount(acc)
	check(1)
	if _, err := setAccountDisabled(acc.AccountID, true); err != nil {
		t.Fatal(err)
	}
	check(0)
	if _, err := setAccountDisabled(acc.AccountID, false); err != nil {
		t.Fatal(err)
	}
	check(1)
	poolMu.Lock()
	acc.Status = "expired"
	poolMu.Unlock()
	check(0)
	poolMu.Lock()
	acc.Status = "active"
	poolMu.Unlock()
	check(1)
	removeAccount(acc.AccountID)
	check(0)
}
