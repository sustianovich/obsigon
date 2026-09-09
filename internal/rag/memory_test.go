package rag

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
)

type memoryTestGenerator struct{ calls int }

func (g *memoryTestGenerator) Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error) {
	g.calls++
	return ollama.GenerationResult{Content: "Resumen acumulativo", PromptTokens: 20, CompletionTokens: 4}, nil
}

func memoryFixture(t *testing.T) (*db.Store, config.Settings) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "memory.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := config.Settings{
		ChatModel: "qwen3.5:0.8b", WriterContextTokens: 16384,
		VerifierContextTokens: 8192, MaxOutputTokens: 1200,
		MemoryRecentTurns: 2, MemoryMaxContextChars: 12000,
		MemorySummaryInterval: 2, MemorySummaryMaxInputChars: 40000,
		MemorySummaryMaxTokens: 700, ProjectMemoryMaxContextChars: 6000,
		ProjectMemorySummaryInterval: 2, ProjectMemorySummaryMaxInputChars: 40000,
		ProjectMemorySummaryMaxTokens: 700,
	}
	return store, settings
}

func TestContextOverviewSeparatesStoredAndActiveHistory(t *testing.T) {
	ctx := context.Background()
	store, settings := memoryFixture(t)
	conversationID := ""
	for index, percent := range []float64{10, 20, 30, 40, 90} {
		metrics := map[string]any{
			"writer":   map[string]any{"status": "completed", "usage_percent": percent},
			"verifier": map[string]any{"status": "completed", "usage_percent": percent - 1},
		}
		question := strings.Repeat("pregunta ", 20+index)
		answer := strings.Repeat("respuesta ", 20+index)
		var err error
		conversationID, _, err = store.SaveConversationTurn(ctx, conversationID, db.DefaultProjectID, question, answer, []map[string]any{}, settings.ChatModel, true, metrics)
		if err != nil {
			t.Fatal(err)
		}
	}
	manager := &MemoryManager{Settings: settings, Store: store}
	overview, err := manager.ContextOverview(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	history := overview["history"].(map[string]any)
	if history["total_turns"].(int) != 5 || history["recent_turns_included"].(int) != 2 || history["stored_estimated_tokens"].(int) <= history["recent_estimated_tokens"].(int) {
		t.Fatalf("historial=%+v", history)
	}
	last := overview["last_inference"].(map[string]any)
	if last["maximum_usage_percent"].(float64) != 90 {
		t.Fatalf("última inferencia=%+v", last)
	}
	next := overview["next_inference_estimate"].(map[string]any)
	if next["effective_context_window"].(int) != 16384 || next["estimated"] != true {
		t.Fatalf("siguiente inferencia=%+v", next)
	}
}

func TestConversationSummaryRunsAtConfiguredCadence(t *testing.T) {
	ctx := context.Background()
	store, settings := memoryFixture(t)
	id, _, err := store.SaveConversationTurn(ctx, "", db.DefaultProjectID, "Primera pregunta", "Primera respuesta", []map[string]any{}, settings.ChatModel, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SaveConversationTurn(ctx, id, db.DefaultProjectID, "Segunda pregunta", "Segunda respuesta", []map[string]any{}, settings.ChatModel, true, nil); err != nil {
		t.Fatal(err)
	}
	generator := &memoryTestGenerator{}
	manager := &MemoryManager{Settings: settings, Store: store, Generator: generator}
	updated, err := manager.MaybeSummarizeConversation(ctx, id)
	if err != nil || !updated || generator.calls != 1 {
		t.Fatalf("updated=%v calls=%d err=%v", updated, generator.calls, err)
	}
	status, err := store.ConversationMemoryStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status["has_summary"] != true || status["summarized_turns"].(int) != 2 || status["pending_turns"].(int) != 0 {
		t.Fatalf("memoria=%+v", status)
	}
}

func TestConcurrentTriggersSummarizeOnlyOnce(t *testing.T) {
	ctx := context.Background()
	store, settings := memoryFixture(t)
	id, _, err := store.SaveConversationTurn(ctx, "", db.DefaultProjectID, "Primera pregunta", "Primera respuesta", []map[string]any{}, settings.ChatModel, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SaveConversationTurn(ctx, id, db.DefaultProjectID, "Segunda pregunta", "Segunda respuesta", []map[string]any{}, settings.ChatModel, true, nil); err != nil {
		t.Fatal(err)
	}
	generator := &memoryTestGenerator{}
	manager := &MemoryManager{Settings: settings, Store: store, Generator: generator}

	// Simula dos peticiones concurrentes sobre la misma conversación (p.ej.
	// un doble envío): el lock por conversación debe evitar que ambas paguen
	// una llamada al LLM por el mismo lote de turnos.
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			manager.TriggerConversationSummary(id)
		}()
	}
	ready.Wait()
	manager.WaitForBackgroundSummaries()

	if generator.calls != 1 {
		t.Fatalf("dos disparos concurrentes deberían resumir una sola vez, calls=%d", generator.calls)
	}
	status, err := store.ConversationMemoryStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status["has_summary"] != true || status["pending_turns"].(int) != 0 {
		t.Fatalf("memoria=%+v", status)
	}
}
