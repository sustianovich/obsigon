package httpapi

import (
	"context"
	"strings"

	"obsigon/internal/rag"
)

// persistedTurn agrupa el estado compartido por la respuesta JSON y el
// evento SSE final. El turno se confirma antes de intentar resumirlo.
type persistedTurn struct {
	ConversationID string
	TurnID         int64
	Memory         map[string]any
	ProjectMemory  map[string]any
}

func (s *Server) persistTurn(ctx context.Context, prepared preparedQuery, answer string, sources []map[string]any, agents map[string]any, hasEvidence bool) (persistedTurn, error) {
	store := s.store()
	conversationID := ""
	if prepared.payload.ConversationID != nil {
		conversationID = strings.TrimSpace(*prepared.payload.ConversationID)
	}
	id, turnID, err := store.SaveConversationTurn(
		ctx, conversationID, prepared.projectID, prepared.payload.Question,
		answer, sources, s.Settings.ChatModel, prepared.payload.UseMemory, agents,
	)
	if err != nil {
		return persistedTurn{}, err
	}

	// El resumen se actualiza en segundo plano (MemoryManager.trigger*): no
	// debe añadir la latencia de una llamada extra al LLM a esta respuesta.
	// Por eso "summary_updated" y "warning" ya no reflejan este request en
	// concreto; el próximo turno leerá el resumen ya actualizado, y un fallo
	// queda registrado en last_error dentro del estado de memoria.
	if hasEvidence && s.Memory != nil && prepared.payload.UseMemory {
		s.Memory.TriggerConversationSummary(id)
	}
	if hasEvidence && s.Memory != nil && prepared.agentSettings.ProjectMemoryEnabled {
		s.Memory.TriggerProjectSummary(prepared.projectID)
	}

	memory := defaultMemoryState(prepared.payload.UseMemory)
	if status, statusErr := store.ConversationMemoryStatus(ctx, id); statusErr == nil {
		memory = status
	} else {
		s.logErrorContext(ctx, "leer el estado de memoria de conversación", statusErr)
	}
	memory["enabled"] = prepared.payload.UseMemory
	memory["used_context"] = strings.TrimSpace(prepared.conversationMemory) != ""
	memory["summary_updated"] = false
	memory["warning"] = nil

	projectMemory := defaultMemoryState(prepared.agentSettings.ProjectMemoryEnabled)
	if status, statusErr := store.ProjectMemoryStatus(ctx, prepared.projectID); statusErr == nil {
		projectMemory = status
	} else {
		s.logErrorContext(ctx, "leer el estado de memoria del proyecto", statusErr)
	}
	projectMemory["enabled"] = prepared.agentSettings.ProjectMemoryEnabled
	projectMemory["used_context"] = strings.TrimSpace(prepared.projectMemory) != ""
	projectMemory["summary_updated"] = false
	projectMemory["warning"] = nil

	return persistedTurn{ConversationID: id, TurnID: turnID, Memory: memory, ProjectMemory: projectMemory}, nil
}

func defaultMemoryState(enabled bool) map[string]any {
	return map[string]any{
		"enabled": enabled, "used_context": false, "has_summary": false,
		"summary_updated": false, "summarized_turns": 0, "pending_turns": 0,
		"total_turns": 0, "summary_model": "", "last_error": nil,
		"warning": nil, "updated_at": nil,
	}
}

func nilIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

type modelContextInfo = rag.ModelContextInfo

func resolveModelContext(ctx context.Context, s *Server) modelContextInfo {
	return rag.ResolveModelContext(ctx, s.Settings.ChatModel, s.Generator)
}

func pipelineOptions(s *Server, prepared preparedQuery, onStage func(string)) rag.AgentOptions {
	return rag.AgentOptions{
		ConversationMemory:      prepared.conversationMemory,
		ProjectMemory:           prepared.projectMemory,
		VerificationEnabled:     prepared.agentSettings.VerificationEnabled,
		VerifierContextTokens:   prepared.agentSettings.VerifierContextTokens,
		WriterContextTokens:     prepared.agentSettings.WriterContextTokens,
		VerifierMaxOutputTokens: s.Settings.VerifierMaxOutputTokens,
		WriterMaxOutputTokens:   s.Settings.MaxOutputTokens,
		AbstainOnInsufficient:   s.Settings.VerifierAbstainOnInsufficient,
		ModelContext:            prepared.modelContext,
		OnStage:                 onStage,
	}
}
