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

func TestDeleteCustomModelCannotDeleteDefaults(t *testing.T) {
	useTemporaryPool(t)

	if err := deleteCustomModel(defaultModels[0].ID); !errors.Is(err, errDefaultModel) {
		t.Fatalf("deleting default model error = %v, want errDefaultModel", err)
	}
	if err := deleteCustomModel("missing/model"); !errors.Is(err, errModelNotFound) {
		t.Fatalf("deleting missing model error = %v, want errModelNotFound", err)
	}
	if _, err := addCustomModel("provider/model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	poolMu.Lock()
	pool.CustomModels = append(pool.CustomModels, "provider/model")
	poolMu.Unlock()
	if err := deleteCustomModel("provider/model"); err != nil {
		t.Fatalf("delete custom model: %v", err)
	}
	if got := len(allModels()); got != len(defaultModels) {
		t.Fatalf("got %d models after deletion, want %d", got, len(defaultModels))
	}
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
	if _, err := addCustomModel("provider/model"); err != nil {
		t.Fatalf("add custom model: %v", err)
	}
	poolPath = t.TempDir()
	if err := deleteCustomModel("provider/model"); !errors.Is(err, errModelStorage) {
		t.Fatalf("delete storage error = %v, want errModelStorage", err)
	}
	if got := loadPool().CustomModels; len(got) != 1 || got[0] != "provider/model" {
		t.Fatalf("failed delete did not roll back: %v", got)
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
