package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const defaultModelContextWindow = 32768

type ModelContextInfo struct {
	Model, Source, Warning string
	ProfileID              any
	Window                 int
	Verified               bool
}

type modelInspector interface {
	ModelShow(context.Context, string) (map[string]any, error)
}

var knownModelContexts = map[string]struct {
	ID     string
	Window int
}{
	"qwen3.5:0.8b":   {"light", 32768},
	"qwen3.5:2b":     {"balanced", 32768},
	"qwen3.5:4b":     {"quality", 32768},
	"qwen3:14b":      {"qwen3_14b", 40960},
	"llama3:latest":  {"llama3", 8192},
	"gpt-oss:latest": {"gpt_oss", 131072},
}

func ResolveModelContext(ctx context.Context, model string, provider any) ModelContextInfo {
	model = strings.TrimSpace(model)
	profile, hasProfile := knownModelContexts[strings.ToLower(model)]
	ollamaWindow := 0
	if inspector, ok := provider.(modelInspector); ok {
		if payload, err := inspector.ModelShow(ctx, model); err == nil {
			ollamaWindow = ContextWindowFromModelInfo(payload["model_info"])
		}
	}
	profileID := any(nil)
	if hasProfile {
		profileID = profile.ID
	}
	if hasProfile && ollamaWindow > 0 {
		window := min(profile.Window, ollamaWindow)
		warning := ""
		if profile.Window != ollamaWindow {
			warning = fmt.Sprintf("El perfil declara %d tokens y Ollama informa %d; se usa el valor más conservador.", profile.Window, ollamaWindow)
		}
		return ModelContextInfo{Model: model, ProfileID: profileID, Window: window, Source: "profile+ollama", Verified: true, Warning: warning}
	}
	if ollamaWindow > 0 {
		return ModelContextInfo{Model: model, ProfileID: profileID, Window: ollamaWindow, Source: "ollama", Verified: true}
	}
	if hasProfile {
		return ModelContextInfo{Model: model, ProfileID: profileID, Window: profile.Window, Source: "profile", Verified: true}
	}
	return ModelContextInfo{Model: model, Window: defaultModelContextWindow, Source: "fallback", Verified: false, Warning: "Modelo sin perfil conocido ni metadatos de Ollama; se aplica un límite prudente de 32768 tokens."}
}

func ContextWindowFromModelInfo(value any) int {
	info, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	best := 0
	for key, raw := range info {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		candidate := 0
		switch number := raw.(type) {
		case float64:
			if number == math.Trunc(number) {
				candidate = int(number)
			}
		case int:
			candidate = number
		case int64:
			candidate = int(number)
		case json.Number:
			parsed, err := number.Int64()
			if err == nil {
				candidate = int(parsed)
			}
		}
		if candidate < 512 || candidate > 10_000_000 {
			continue
		}
		if best == 0 || candidate < best {
			best = candidate
		}
	}
	return best
}
