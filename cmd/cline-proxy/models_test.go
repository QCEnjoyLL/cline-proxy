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
	pool = nil
	poolPath = filepath.Join(t.TempDir(), ".cline-accounts.json")
	t.Cleanup(func() {
		pool = originalPool
		poolPath = originalPath
	})
}

func TestCustomModelsPreserveDefaultsAndPersist(t *testing.T) {
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
	if len(models) != len(defaultModels)+1 {
		t.Fatalf("got %d models, want %d", len(models), len(defaultModels)+1)
	}
	for i, defaultModel := range defaultModels {
		if models[i].ID != defaultModel.ID || models[i].Custom {
			t.Fatalf("default model %d was not preserved: %+v", i, models[i])
		}
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

	if _, err := addCustomModel(defaultModels[0].ID); !errors.Is(err, errModelExists) {
		t.Fatalf("adding default model error = %v, want errModelExists", err)
	}
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

func TestDeletePresetModelsAllowedAndRestorable(t *testing.T) {
	useTemporaryPool(t)

	preset := defaultModels[0].ID
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
	if got := len(allModels()); got != len(defaultModels) {
		t.Fatalf("got %d models after deletion, want %d", got, len(defaultModels))
	}
}

func TestDeletePresetModelsPersistsAcrossReload(t *testing.T) {
	useTemporaryPool(t)

	preset := defaultModels[0].ID
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

	// Default points at the first preset; deleting it must fall back to the
	// next available model instead of the deleted one.
	preset := defaultModels[0].ID
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
	if err := setDefaultModel(defaultModels[1].ID); !errors.Is(err, errModelStorage) {
		t.Fatalf("default model storage error = %v, want errModelStorage", err)
	}
	if got := getDefaultModel(); got != defaultModel {
		t.Fatalf("failed default update was not rolled back: %q", got)
	}

	poolPath = originalPath
	if _, err := addCustomModel("provider/model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	if err := setDefaultModel("provider/model"); err != nil {
		t.Fatalf("set custom default model: %v", err)
	}
	poolPath = t.TempDir()
	if err := deleteCustomModel("provider/model"); !errors.Is(err, errModelStorage) {
		t.Fatalf("delete storage error = %v, want errModelStorage", err)
	}
	if got := loadPool().CustomModels; len(got) != 1 || got[0] != "provider/model" {
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
	if !body.Success || len(body.Data.Models) != len(defaultModels)+1 {
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
	if got := getDefaultModel(); got != defaultModel {
		t.Fatalf("initial default model = %q, want %q", got, defaultModel)
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
	if got := getDefaultModel(); got != defaultModel {
		t.Fatalf("default after custom deletion = %q, want %q", got, defaultModel)
	}
	pool = nil
	if got := getDefaultModel(); got != defaultModel {
		t.Fatalf("persisted fallback = %q, want %q", got, defaultModel)
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
