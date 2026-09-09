package db

import (
	"context"
	"testing"
)

func TestConversationMetricsMarkLegacyContextAsIncomplete(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	id, _, err := store.SaveConversationTurn(ctx, "", DefaultProjectID, "Pregunta", "Respuesta", []map[string]any{}, "modelo-antiguo", true, map[string]any{
		"writer": map[string]any{"status": "completed", "usage_percent": 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	turn := conversation["turns"].([]map[string]any)[0]
	metrics := turn["agent_metrics"].(map[string]any)
	writer := metrics["writer"].(map[string]any)
	if writer["incomplete_context_data"] != true {
		t.Fatalf("métricas=%+v", metrics)
	}
}

func TestConversationMetricsPreserveCompleteHistoricalContext(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	role := map[string]any{
		"status": "completed", "model": "llama3:latest",
		"model_context_window": 8192, "configured_context_window": 8192,
		"effective_context_window": 8192, "context_source": "profile",
		"usage_percent": 20,
	}
	id, _, err := store.SaveConversationTurn(ctx, "", DefaultProjectID, "Pregunta", "Respuesta", []map[string]any{}, "llama3:latest", true, map[string]any{"writer": role})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	writer := conversation["turns"].([]map[string]any)[0]["agent_metrics"].(map[string]any)["writer"].(map[string]any)
	if writer["incomplete_context_data"] != false || writer["model_context_window"].(float64) != 8192 {
		t.Fatalf("métricas=%+v", writer)
	}
}

func TestDeleteConversationInvalidatesProjectMemory(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	first, _, err := store.SaveConversationTurn(ctx, "", DefaultProjectID, "Primera", "Respuesta", []map[string]any{}, "modelo", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, lastTurn, err := store.SaveConversationTurn(ctx, "", DefaultProjectID, "Segunda", "Respuesta", []map[string]any{}, "modelo", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := store.SaveMemorySummary(ctx, "project", DefaultProjectID, "Resumen compartido", int(lastTurn), 2, "modelo"); err != nil || !saved {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	deleted, err := store.DeleteConversation(ctx, first)
	if err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	status, err := store.ProjectMemoryStatus(ctx, DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if status["has_summary"] != false || status["total_turns"].(int) != 1 || status["pending_turns"].(int) != 1 {
		t.Fatalf("memoria no invalidada: %+v", status)
	}
}

func TestProjectContextUsageAggregatesRolesWithoutAddingTurns(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	conversationID := ""
	for _, percent := range []int{10, 90} {
		metrics := map[string]any{
			"writer":   map[string]any{"status": "completed", "context_window_tokens": 1000, "prompt_tokens": percent * 10, "completion_tokens": 0, "total_tokens": percent * 10, "usage_percent": percent},
			"verifier": map[string]any{"status": "disabled", "context_window_tokens": 1000},
		}
		var err error
		conversationID, _, err = store.SaveConversationTurn(ctx, conversationID, DefaultProjectID, "Pregunta", "Respuesta", []map[string]any{}, "modelo", true, metrics)
		if err != nil {
			t.Fatal(err)
		}
	}
	usage, err := store.ProjectContextUsage(ctx, DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	writer := usage["writer"].(map[string]any)
	verifier := usage["verifier"].(map[string]any)
	if writer["samples"].(int) != 2 || writer["maximum_percent"].(float64) != 90 || writer["average_percent"].(float64) != 50 || usage["latest_percent"].(float64) != 90 {
		t.Fatalf("uso=%+v", usage)
	}
	if verifier["samples"].(int) != 0 {
		t.Fatalf("verificador=%+v", verifier)
	}
}

func TestConversationAndTurnPagination(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	conversationIDs := []string{}
	for index := 0; index < 3; index++ {
		id, _, err := store.SaveConversationTurn(ctx, "", DefaultProjectID, "Pregunta", "Respuesta", []map[string]any{}, "modelo", true, nil)
		if err != nil {
			t.Fatal(err)
		}
		conversationIDs = append(conversationIDs, id)
	}
	first, total, err := store.ListConversationsPage(ctx, nil, 2, 0)
	if err != nil || total != 3 || len(first) != 2 {
		t.Fatalf("first=%+v total=%d err=%v", first, total, err)
	}
	second, total, err := store.ListConversationsPage(ctx, nil, 2, 2)
	if err != nil || total != 3 || len(second) != 1 {
		t.Fatalf("second=%+v total=%d err=%v", second, total, err)
	}
	id := conversationIDs[0]
	for index := 0; index < 4; index++ {
		if _, _, err := store.SaveConversationTurn(ctx, id, DefaultProjectID, "Seguimiento", "Respuesta", []map[string]any{}, "modelo", true, nil); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.GetConversationPage(ctx, id, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	turns := page["turns"].([]map[string]any)
	meta := page["turn_page"].(map[string]any)
	if len(turns) != 2 || meta["total"] != 5 || meta["has_more"] != true {
		t.Fatalf("page=%+v", page)
	}
	next := meta["next_before_id"].(int64)
	older, err := store.GetConversationPage(ctx, id, next, 2)
	if err != nil || len(older["turns"].([]map[string]any)) != 2 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
}
