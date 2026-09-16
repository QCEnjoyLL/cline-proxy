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
	errModelNotFound = errors.New("model not found")
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

// isDefaultModelID reports whether id is one of the built-in models.
func isDefaultModelID(id string) bool {
	for _, model := range defaultModels {
		if model.ID == id {
			return true
		}
	}
	return false
}

// modelDisabledLocked reports whether a built-in model has been deleted
// (disabled) by the user. Callers must hold poolMu.
func modelDisabledLocked(p *AccountPool, id string) bool {
	for _, disabledID := range p.DisabledModels {
		if disabledID == id {
			return true
		}
	}
	return false
}

// firstAvailableModelLocked returns the first model the pool can use: the
// first non-disabled built-in model, or the first custom model when every
// built-in model has been deleted. Returns "" when no models remain.
// Callers must hold poolMu.
func firstAvailableModelLocked(p *AccountPool) string {
	for _, model := range defaultModels {
		if !modelDisabledLocked(p, model.ID) {
			return model.ID
		}
	}
	if len(p.CustomModels) > 0 {
		return p.CustomModels[0]
	}
	return ""
}

func modelExistsLocked(p *AccountPool, id string) bool {
	if isDefaultModelID(id) && !modelDisabledLocked(p, id) {
		return true
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
	return firstAvailableModelLocked(p)
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
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	models := make([]modelDefinition, 0, len(defaultModels)+len(p.CustomModels))
	seen := make(map[string]struct{}, len(defaultModels)+len(p.CustomModels))
	for _, model := range defaultModels {
		if modelDisabledLocked(p, model.ID) {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
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

	// Built-in model: adding it back re-enables it after a deletion.
	if isDefaultModelID(id) {
		if !modelDisabledLocked(p, id) {
			return "", errModelExists
		}
		original := append([]string(nil), p.DisabledModels...)
		filtered := make([]string, 0, len(p.DisabledModels))
		for _, disabledID := range p.DisabledModels {
			if disabledID == id {
				continue
			}
			filtered = append(filtered, disabledID)
		}
		p.DisabledModels = filtered
		if err := savePool(); err != nil {
			p.DisabledModels = original
			return "", fmt.Errorf("%w: %v", errModelStorage, err)
		}
		return id, nil
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

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	// Built-in model: deletion disables it; re-adding restores it.
	if isDefaultModelID(id) {
		if modelDisabledLocked(p, id) {
			return errModelNotFound
		}
		original := append([]string(nil), p.DisabledModels...)
		originalDefault := p.DefaultModel
		p.DisabledModels = append(p.DisabledModels, id)
		if p.DefaultModel == id {
			p.DefaultModel = ""
		}
		if err := savePool(); err != nil {
			p.DisabledModels = original
			p.DefaultModel = originalDefault
			return fmt.Errorf("%w: %v", errModelStorage, err)
		}
		return nil
	}

	// Custom model: remove it from the custom list.
	original := append([]string(nil), p.CustomModels...)
	originalDefault := p.DefaultModel
	filtered := make([]string, 0, len(p.CustomModels))
	found := false
	for _, customID := range p.CustomModels {
		normalizedID, normalizeErr := normalizeModelID(customID)
		if normalizeErr == nil && normalizedID == id {
			found = true
			continue
		}
		filtered = append(filtered, customID)
	}
	if !found {
		return errModelNotFound
	}
	p.CustomModels = filtered
	if p.DefaultModel == id {
		p.DefaultModel = ""
	}
	if err := savePool(); err != nil {
		p.CustomModels = original
		p.DefaultModel = originalDefault
		return fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return nil
}
