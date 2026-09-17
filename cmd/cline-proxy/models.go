package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxModelIDLength = 200

// modelIDForbiddenRunes 是 model ID 里一律禁止的字符。
//
// 选取原则：只禁掉“不可能出现在任何模型标识里、但会破坏下游字符串语法”的字符，
// 避免收得过紧误伤真实模型名——像 openai/gpt-4.1-nano、cline-pass/qwen3.7-max
// 这类含 / . - 的 ID 必须照常可用。
const modelIDForbiddenRunes = "|'\"`\\<>"

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
		// 归一化只用于「新增」时把关。这里面对的是**已经存下来的**历史数据：
		// 1.3.4 之前允许含 | 引号 等字符的 ID 直接入库，它们如今过不了
		// normalizeModelID。此时必须退回原始字符串比较，否则这些旧 ID 会
		// 变得「列得出来、却删不掉、也设不成默认模型」。
		normalizedID, err := normalizeModelID(customID)
		if err != nil {
			normalizedID = customID
		}
		if normalizedID == id {
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
	// 这里的目标 ID 必须是池子里**已存在**的模型，所以走 normalizeExistingModelID：
	// 新增路径才需要字符集把关，收紧校验不该让历史 ID 永远当不上默认模型。
	id = normalizeExistingModelID(id)

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	if !modelExistsLocked(p, id) {
		return errModelUnknown
	}

	previous := p.DefaultModel
	p.DefaultModel = id
	if err := savePoolLocked(); err != nil {
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
	// model ID 会被拼进两处“有语法的字符串”，因此不能放任任意字符：
	//
	//  1. 管理面板把它内插进 HTML 属性里的 onclick（clearCooldown('...')），
	//     引号能提前闭合属性、反斜杠能吃掉转移符——即注入 JS。
	//  2. 冷却键是 accountID + "|" + modelID（见 cooldown.go），含 "|" 会让
	//     splitCooldownKey 切错位置，该条冷却从此不在面板上显示。
	//
	// 已有的 esc() 只做 HTML 转义（&<>），挡不住上面两种；所以在这一层
	// 直接禁掉这些字符，作为唯一的把关点。
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("model ID cannot contain whitespace or control characters")
		}
		if strings.ContainsRune(modelIDForbiddenRunes, r) {
			return "", fmt.Errorf("model ID cannot contain any of %s", modelIDForbiddenRunes)
		}
	}
	return id, nil
}

// normalizeExistingModelID 用于**已经在池子里**的 ID（设置默认模型、删除等），
// 与新增路径的 normalizeModelID 分开：
//
// 新数据必须过字符集校验（否则会带进注入/冷却键问题）；但历史数据是既成事实，
// 校验收紧不该把它变成「删不掉的孤儿」——1.3.4 之前允许含 | 引号 的 ID 入库。
// 那些 ID 只做长度/空白这类「不会因版本变化而失效」的清理，校验失败即原样返回。
func normalizeExistingModelID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return id
	}
	return trimmed
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
		if err := savePoolLocked(); err != nil {
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
	if err := savePoolLocked(); err != nil {
		p.CustomModels = p.CustomModels[:len(p.CustomModels)-1]
		return "", fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return id, nil
}

func deleteCustomModel(id string) error {
	// 同 setDefaultModel：删除是清理历史数据的唯一出口，绝不能被收紧后的
	// 字符集校验挡在门外，否则旧 ID 会永久占位、删不掉。
	id = normalizeExistingModelID(id)

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
		if err := savePoolLocked(); err != nil {
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
		// 同 modelExistsLocked：历史 ID 可能过不了校验，这时按原样比较，
		// 保证旧数据依然可以被删除（deleteCustomModel 是清理它们的唯一出口）。
		normalizedID, normalizeErr := normalizeModelID(customID)
		if normalizeErr != nil {
			normalizedID = customID
		}
		if normalizedID == id {
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
	if err := savePoolLocked(); err != nil {
		p.CustomModels = original
		p.DefaultModel = originalDefault
		return fmt.Errorf("%w: %v", errModelStorage, err)
	}
	return nil
}
