package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTemporaryPool(t *testing.T) {
	t.Helper()
	originalPool, originalPath := pool, poolPath
	originalConfig := getProxyConfig()
	pool = nil
	poolPath = filepath.Join(t.TempDir(), ".cline-accounts.json")
	t.Cleanup(func() {
		pool = originalPool
		poolPath = originalPath
		setProxyConfig(originalConfig)
	})
}

func TestCustomModelsPersistWithoutPresets(t *testing.T) {
	useTemporaryPool(t)

	id, err := addCustomModel("  openai/gpt-4.1-nano  ")
	if err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	if id != "openai/gpt-4.1-nano" {
		t.Fatalf("normalized ID = %q", id)
	}

	pool = nil // Force a reload from disk.
	models := allModels()
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	custom := models[len(models)-1]
	if custom.ID != id || !custom.Custom {
		t.Fatalf("custom model was not restored: %+v", custom)
	}
	if _, err := os.Stat(poolPath); err != nil {
		t.Fatalf("pool file was not persisted: %v", err)
	}
}

func TestCustomModelRejectsDuplicatesAndInvalidIDs(t *testing.T) {
	useTemporaryPool(t)

	if _, err := addCustomModel("provider/model"); err != nil {
		t.Fatalf("add first custom model: %v", err)
	}
	if _, err := addCustomModel("provider/model"); !errors.Is(err, errModelExists) {
		t.Fatalf("adding duplicate custom model error = %v, want errModelExists", err)
	}

	invalid := []string{"", "   ", "provider/model name", "provider/model\nname", strings.Repeat("a", maxModelIDLength+1)}
	for _, id := range invalid {
		if _, err := addCustomModel(id); err == nil {
			t.Errorf("addCustomModel(%q) unexpectedly succeeded", id)
		}
	}
}

// model ID 会进入两处“有语法的字符串”：面板 onclick 里的 JS 字面量，以及
// 「accountID|modelID」形式的冷却键。这里锁住字符集把关点。
//
// 背景：这两个位置曾被引号 / 竖线打穿——esc() 只做 HTML 转义不挡 JS 注入，
// 而含 "|" 的 ID 会让 splitCooldownKey 切错，冷却条目在面板上整条消失。
func TestNormalizeModelIDRejectsSyntaxBreakingRunes(t *testing.T) {
	for _, bad := range []string{
		// 闭合 onclick="..." 属性
		`openai/a"b`,
		// 逃出 JS 字符串字面量
		`openai/a'b`,
		// 反斜杠吃掉转移符
		`openai/a\b`,
		// 冷却键分隔符
		"openai/a|b",
		// 提前结束 <script> 上下文
		"openai/a<b",
		// 反引号（模板字面量）
		"openai/a`b",
	} {
		if _, err := normalizeModelID(bad); err == nil {
			t.Errorf("normalizeModelID(%q) unexpectedly succeeded", bad)
		}
	}

	// 真实模型名不能被误伤：斜杠、点、连字符、冒号都必须照常可用。
	for _, good := range []string{
		"openai/gpt-4.1-nano",
		"cline-pass/qwen3.7-max",
		"deepseek/deepseek-v4-pro",
		"google/gemini-2.5-flash",
		"provider:model/v1",
	} {
		if got, err := normalizeModelID(good); err != nil || got != good {
			t.Errorf("normalizeModelID(%q) = %q, %v; want unchanged", good, got, err)
		}
	}
}

func TestFormerPresetModelCanBeAddedRemovedAndRestored(t *testing.T) {
	useTemporaryPool(t)

	preset := "cline-free/glm-5.2"
	if _, err := addCustomModel(preset); err != nil {
		t.Fatalf("add former preset as a user model: %v", err)
	}
	if err := deleteCustomModel(preset); err != nil {
		t.Fatalf("delete preset model: %v", err)
	}
	for _, m := range allModels() {
		if m.ID == preset {
			t.Fatalf("deleted preset model still listed: %+v", m)
		}
	}
	if err := deleteCustomModel(preset); !errors.Is(err, errModelNotFound) {
		t.Fatalf("deleting already-deleted preset model error = %v, want errModelNotFound", err)
	}
	if err := deleteCustomModel("missing/model"); !errors.Is(err, errModelNotFound) {
		t.Fatalf("deleting missing model error = %v, want errModelNotFound", err)
	}

	// Re-adding the preset ID restores it.
	if id, err := addCustomModel(preset); err != nil || id != preset {
		t.Fatalf("restore preset model: id=%q err=%v", id, err)
	}
	found := false
	for _, m := range allModels() {
		if m.ID == preset {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored preset model %q not listed", preset)
	}
	if _, err := addCustomModel(preset); !errors.Is(err, errModelExists) {
		t.Fatalf("re-adding active preset model error = %v, want errModelExists", err)
	}

	// Custom models still work as before.
	if _, err := addCustomModel("provider/model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	if err := deleteCustomModel("provider/model"); err != nil {
		t.Fatalf("delete custom model: %v", err)
	}
	if got := len(allModels()); got != 1 {
		t.Fatalf("got %d models after deletion, want 1", got)
	}
}

func TestDeletedModelsStayDeletedAcrossReload(t *testing.T) {
	useTemporaryPool(t)

	preset := "provider/model"
	if _, err := addCustomModel(preset); err != nil {
		t.Fatal(err)
	}
	if err := deleteCustomModel(preset); err != nil {
		t.Fatalf("delete preset model: %v", err)
	}
	pool = nil // Force a reload from disk.
	for _, m := range allModels() {
		if m.ID == preset {
			t.Fatalf("deleted preset model survived reload: %+v", m)
		}
	}
	if id, err := addCustomModel(preset); err != nil || id != preset {
		t.Fatalf("restore preset after reload: id=%q err=%v", id, err)
	}
	pool = nil
	found := false
	for _, m := range allModels() {
		if m.ID == preset {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored preset model %q did not survive reload", preset)
	}
}

func TestDeleteDefaultModelFallsBackToAvailableModel(t *testing.T) {
	useTemporaryPool(t)

	// The first user-added model is the fallback until a default is selected.
	preset := "provider/first"
	if _, _, failed := addCustomModels([]string{preset, "provider/second"}); len(failed) != 0 {
		t.Fatalf("seed models: %v", failed)
	}
	if got := getDefaultModel(); got != preset {
		t.Fatalf("initial default model = %q, want %q", got, preset)
	}
	if err := deleteCustomModel(preset); err != nil {
		t.Fatalf("delete default preset model: %v", err)
	}
	got := getDefaultModel()
	if got == preset || got == "" {
		t.Fatalf("default after deleting %q = %q, want a different available model", preset, got)
	}
	for _, m := range allModels() {
		if m.ID == got {
			return
		}
	}
	t.Fatalf("fallback default %q is not listed", got)
}

func TestCustomModelChangesRollBackWhenPersistenceFails(t *testing.T) {
	useTemporaryPool(t)

	originalPath := poolPath
	poolPath = t.TempDir() // os.WriteFile cannot replace a directory.
	if _, err := addCustomModel("provider/add-fails"); !errors.Is(err, errModelStorage) {
		t.Fatalf("add storage error = %v, want errModelStorage", err)
	}
	if got := len(loadPool().CustomModels); got != 0 {
		t.Fatalf("failed add left %d custom models in memory", got)
	}
	poolPath = originalPath
	if _, _, failed := addCustomModels([]string{"provider/model", "provider/other"}); len(failed) != 0 {
		t.Fatalf("seed models: %v", failed)
	}
	if err := setDefaultModel("provider/model"); err != nil {
		t.Fatal(err)
	}
	poolPath = t.TempDir()
	if err := setDefaultModel("provider/other"); !errors.Is(err, errModelStorage) {
		t.Fatalf("default model storage error = %v, want errModelStorage", err)
	}
	if got := getDefaultModel(); got != "provider/model" {
		t.Fatalf("failed default update was not rolled back: %q", got)
	}

	if err := deleteCustomModel("provider/model"); !errors.Is(err, errModelStorage) {
		t.Fatalf("delete storage error = %v, want errModelStorage", err)
	}
	if got := loadPool().CustomModels; len(got) != 2 || got[0] != "provider/model" || got[1] != "provider/other" {
		t.Fatalf("failed delete did not roll back: %v", got)
	}
	if got := getDefaultModel(); got != "provider/model" {
		t.Fatalf("failed delete did not restore default model: %q", got)
	}
}

func TestAdminModelHandlers(t *testing.T) {
	useTemporaryPool(t)

	addRequest := httptest.NewRequest(http.MethodPost, "/admin/api/models", strings.NewReader(`{"id":"provider/model"}`))
	addResponse := httptest.NewRecorder()
	handleAdminModels(addResponse, addRequest)
	if addResponse.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want %d; body=%s", addResponse.Code, http.StatusCreated, addResponse.Body.String())
	}

	duplicateResponse := httptest.NewRecorder()
	handleAdminModels(duplicateResponse, httptest.NewRequest(http.MethodPost, "/admin/api/models", strings.NewReader(`{"id":"provider/model"}`)))
	if duplicateResponse.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want %d", duplicateResponse.Code, http.StatusConflict)
	}

	listResponse := httptest.NewRecorder()
	handleAdminModels(listResponse, httptest.NewRequest(http.MethodGet, "/admin/api/models", nil))
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Models []modelDefinition `json:"models"`
		} `json:"data"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&body); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if !body.Success || len(body.Data.Models) != 1 {
		t.Fatalf("unexpected list response: %+v", body)
	}

	deleteResponse := httptest.NewRecorder()
	handleAdminModelDelete(deleteResponse, httptest.NewRequest(http.MethodPost, "/admin/api/models/delete", strings.NewReader(`{"id":"provider/model"}`)))
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d; body=%s", deleteResponse.Code, http.StatusOK, deleteResponse.Body.String())
	}
}

func TestDefaultModelSelectionPersistsAndDrivesRequests(t *testing.T) {
	useTemporaryPool(t)
	if got := getDefaultModel(); got != "" {
		t.Fatalf("initial default model = %q, want empty", got)
	}
	if _, err := addCustomModel("provider/custom-model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	if err := setDefaultModel("provider/custom-model"); err != nil {
		t.Fatalf("set default model: %v", err)
	}

	pool = nil
	if got := getDefaultModel(); got != "provider/custom-model" {
		t.Fatalf("persisted default model = %q", got)
	}
	if got := buildUpstreamBody(map[string]any{}, false)["model"]; got != "provider/custom-model" {
		t.Fatalf("upstream default model = %v", got)
	}
	if got := buildUpstreamBody(map[string]any{"model": "request/model"}, false)["model"]; got != "request/model" {
		t.Fatalf("request model did not override default: %v", got)
	}
}

func TestDefaultModelValidationAndCustomDeletionFallback(t *testing.T) {
	useTemporaryPool(t)
	if err := setDefaultModel("missing/model"); !errors.Is(err, errModelUnknown) {
		t.Fatalf("unknown default error = %v, want errModelUnknown", err)
	}
	if _, err := addCustomModel("provider/custom-model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	if err := setDefaultModel("provider/custom-model"); err != nil {
		t.Fatalf("set default model: %v", err)
	}
	if err := deleteCustomModel("provider/custom-model"); err != nil {
		t.Fatalf("delete custom model: %v", err)
	}
	if got := getDefaultModel(); got != "" {
		t.Fatalf("default after custom deletion = %q, want empty", got)
	}
	pool = nil
	if got := getDefaultModel(); got != "" {
		t.Fatalf("persisted fallback = %q, want empty", got)
	}
}

func TestAdminDefaultModelConfig(t *testing.T) {
	useTemporaryPool(t)
	if _, err := addCustomModel("provider/custom-model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}

	response := httptest.NewRecorder()
	handleAdminUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/admin/api/config/update", strings.NewReader(`{"defaultModel":"provider/custom-model"}`)))
	if response.Code != http.StatusOK || getDefaultModel() != "provider/custom-model" {
		t.Fatalf("config update status=%d default=%q body=%s", response.Code, getDefaultModel(), response.Body.String())
	}
	var body struct {
		Data struct {
			DefaultModel string `json:"defaultModel"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body.Data.DefaultModel != "provider/custom-model" {
		t.Fatalf("config response default=%q err=%v", body.Data.DefaultModel, err)
	}

	invalidResponse := httptest.NewRecorder()
	handleAdminUpdateConfig(invalidResponse, httptest.NewRequest(http.MethodPost, "/admin/api/config/update", strings.NewReader(`{"defaultModel":"missing/model"}`)))
	if invalidResponse.Code != http.StatusBadRequest || getDefaultModel() != "provider/custom-model" {
		t.Fatalf("invalid config status=%d default=%q", invalidResponse.Code, getDefaultModel())
	}
}

// 收紧字符集不得让历史数据变成「删不掉的孤儿」。
//
// 1.3.4 之前 addCustomModel 不检查 | 引号 等字符，这类 ID 可能已经躺在
// .cline-accounts.json 里。校验收紧后它们过不了 normalizeModelID，若
// modelExistsLocked / deleteCustomModel 只认归一化后的结果，就会：
//   - 列表里看得见，却设不成默认模型（modelExistsLocked 认为它不存在）；
//   - 也删不掉（deleteCustomModel 报 errModelNotFound），永久占位。
//
// 这两处面对历史数据时必须退回原始字符串比较。
func TestLegacyModelIDsStayListedAndDeletable(t *testing.T) {
	useTemporaryPool(t)

	// 模拟旧版本写进池里的、如今过不了校验的 ID。
	const legacy = `vendor/legacy|x"y`
	if _, err := normalizeModelID(legacy); err == nil {
		t.Fatalf("前提不成立：normalizeModelID 仍接受 %q，本测试无意义", legacy)
	}

	p := loadPool()
	poolMu.Lock()
	p.CustomModels = append(p.CustomModels, legacy)
	poolMu.Unlock()
	if err := savePool(); err != nil {
		t.Fatalf("persist legacy model: %v", err)
	}

	// 1) 仍被认定为存在（否则无法设为默认模型）。
	p = loadPool()
	poolMu.Lock()
	exists := modelExistsLocked(p, legacy)
	poolMu.Unlock()
	if !exists {
		t.Error("历史 ID 应仍被视为存在，否则设不成默认模型")
	}

	// 2) 仍可设置成默认模型。
	if err := setDefaultModel(legacy); err != nil {
		t.Errorf("历史 ID 应仍可设为默认模型：%v", err)
	}

	// 3) 仍可删除——这是清理旧数据的唯一出口。
	if err := deleteCustomModel(legacy); err != nil {
		t.Fatalf("历史 ID 应仍可删除，否则会永久占位：%v", err)
	}
	for _, m := range allModels() {
		if m.ID == legacy {
			t.Errorf("删除后历史 ID 仍出现在模型列表：%+v", m)
		}
	}
}
