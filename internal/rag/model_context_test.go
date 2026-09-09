package rag

import (
	"context"
	"errors"
	"testing"
)

type contextInspector struct {
	info map[string]any
	err  error
}

func (i contextInspector) ModelShow(context.Context, string) (map[string]any, error) {
	if i.err != nil {
		return nil, i.err
	}
	return map[string]any{"model_info": i.info}, nil
}

func TestResolveModelContextUsesConservativeValue(t *testing.T) {
	info := ResolveModelContext(context.Background(), "llama3:latest", contextInspector{info: map[string]any{"llama.context_length": float64(4096)}})
	if info.Window != 4096 || info.Source != "profile+ollama" || !info.Verified || info.Warning == "" {
		t.Fatalf("contexto=%+v", info)
	}
}

func TestResolveModelContextFallsBackWithoutProfileOrMetadata(t *testing.T) {
	info := ResolveModelContext(context.Background(), "modelo-desconocido", contextInspector{err: errors.New("apagado")})
	if info.Window != 32768 || info.Source != "fallback" || info.Verified || info.Warning == "" {
		t.Fatalf("contexto=%+v", info)
	}
}

func TestContextWindowRejectsInvalidAndUsesSmallestValue(t *testing.T) {
	window := ContextWindowFromModelInfo(map[string]any{"a.context_length": float64(16384), "b.context_length": 8192, "invalid.context_length": true, "tiny.context_length": 10})
	if window != 8192 {
		t.Fatalf("window=%d", window)
	}
}
