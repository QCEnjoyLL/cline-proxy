package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxModelIDLength = 200

var (
	errModelExists   = errors.New("model already exists")
	errModelNotFound = errors.New("custom model not found")
	errDefaultModel  = errors.New("default models cannot be deleted")
	errModelStorage  = errors.New("persist model configuration")
	errModelUnknown  = errors.New("model is not configured")
)

type modelDefinition struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Cost     string `json:"cost"`
	Status   string `json:"status"`
	Custom   bool   `json:"custom"`
}

var defaultModels = []modelDefinition{
	{ID: defaultModel, Provider: "zai", Cost: "free", Status: "active"},
	{ID: "cline-pass/glm-5.2", Provider: "zai", Cost: "pass", Status: "active"},
	{ID: "cline-pass/deepseek-v4-flash", Provider: "deepseek", Cost: "pass", Status: "active"},
	{ID: "cline-pass/qwen3.7-max", Provider: "qwen", Cost: "pass", Status: "active"},
}

func modelExistsLocked(p *AccountPool, id string) bool {
	for _, model := range defaultModels {
		if model.ID == id {
			return true
		}
	}
	for _, customID := range p.CustomModels {
		normalizedID, err := normalizeModelID(customID)
		if err == nil && normalizedID == id {
			return true
		}
	}
	return false
}

func getDefaultModel() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	if p.DefaultModel != "" && modelExistsLocked(p, p.DefaultModel) {
		return p.DefaultModel
	}
	return defaultModel
}

func setDefaultModel(id string) error {
	id, err := normalizeModelID(id)
	if err != nil {
		return err
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	if !modelExistsLocked(p, id) {
		return errModelUnknown
	}

	previous := p.DefaultModel
	p.DefaultModel = id
	if err := savePool(); err != nil {
		p.DefaultModel = previous
		return fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return nil
}

func allModels() []modelDefinition {
	models := make([]modelDefinition, len(defaultModels))
	copy(models, defaultModels)

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	seen := make(map[string]struct{}, len(defaultModels)+len(p.CustomModels))
	for _, model := range defaultModels {
		seen[model.ID] = struct{}{}
	}
	for _, id := range p.CustomModels {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, modelDefinition{
			ID:       id,
			Provider: "custom",
			Cost:     "custom",
			Status:   "active",
			Custom:   true,
		})
	}
	return models
}

func normalizeModelID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("model ID is required")
	}
	if !utf8.ValidString(id) {
		return "", errors.New("model ID must be valid UTF-8")
	}
	if len(id) > maxModelIDLength {
		return "", fmt.Errorf("model ID must not exceed %d bytes", maxModelIDLength)
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("model ID cannot contain whitespace or control characters")
		}
	}
	return id, nil
}

func addCustomModel(id string) (string, error) {
	id, err := normalizeModelID(id)
	if err != nil {
		return "", err
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, model := range defaultModels {
		if model.ID == id {
			return "", errModelExists
		}
	}
	for _, customID := range p.CustomModels {
		if customID == id {
			return "", errModelExists
		}
	}
	p.CustomModels = append(p.CustomModels, id)
	if err := savePool(); err != nil {
		p.CustomModels = p.CustomModels[:len(p.CustomModels)-1]
		return "", fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return id, nil
}

func deleteCustomModel(id string) error {
	id, err := normalizeModelID(id)
	if err != nil {
		return err
	}
	for _, model := range defaultModels {
		if model.ID == id {
			return errDefaultModel
		}
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	original := append([]string(nil), p.CustomModels...)
	originalDefault := p.DefaultModel
	filtered := make([]string, 0, len(p.CustomModels))
	for _, customID := range p.CustomModels {
		normalizedID, normalizeErr := normalizeModelID(customID)
		if normalizeErr == nil && normalizedID == id {
			continue
		}
		filtered = append(filtered, customID)
	}
	if len(filtered) == len(p.CustomModels) {
		return errModelNotFound
	}
	p.CustomModels = filtered
	if p.DefaultModel == id {
		p.DefaultModel = defaultModel
	}
	if err := savePool(); err != nil {
		p.CustomModels = original
		p.DefaultModel = originalDefault
		return fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return nil
}
