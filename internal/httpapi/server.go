package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	webui "obsigon/app"
	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

type Generator interface {
	Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error)
	StreamChat(context.Context, string, string, ollama.ChatOptions, func(string) error) (ollama.GenerationResult, error)
}

type statusInspector interface {
	Status(context.Context) map[string]any
}

type VaultPicker func(context.Context, string) (path string, selected bool, err error)

type Server struct {
	Settings    config.Settings
	Store       *db.Store
	Indexer     *rag.Indexer
	Retriever   *rag.Retriever
	Generator   Generator
	Memory      *rag.MemoryManager
	Progress    *IndexProgress
	Watcher     *rag.VaultWatcher
	VaultPicker VaultPicker
	Logger      *slog.Logger
	Build       BuildInfo
	traceMu     sync.RWMutex
	lastTrace   *storedRetrievalTrace
	// runtimeMu mantiene coherentes Settings y los componentes que dependen
	// del vault/modelo activo. Las consultas conservan un RLock durante toda
	// la petición; los cambios de configuración esperan a que terminen antes
	// de publicar el nuevo runtime.
	runtimeMu sync.RWMutex
}

type storedRetrievalTrace struct {
	CreatedAt time.Time          `json:"created_at"`
	Trace     rag.RetrievalTrace `json:"trace"`
}

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "kind": "liveness"})
	})
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "kind": "liveness"})
	})
	mux.HandleFunc("GET /health/ready", s.readiness)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/config/chat-profile", s.selectChatProfile)
	mux.HandleFunc("POST /api/vault/pick", s.pickVault)
	mux.HandleFunc("POST /api/vault/select", s.selectVault)
	mux.HandleFunc("POST /api/index", s.index)
	mux.HandleFunc("POST /api/index/start", s.startIndex)
	mux.HandleFunc("POST /api/index/cancel", s.cancelIndex)
	mux.HandleFunc("GET /api/index/progress", s.indexProgress)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("PATCH /api/projects/{project_id}", s.renameProject)
	mux.HandleFunc("DELETE /api/projects/{project_id}", s.deleteProject)
	mux.HandleFunc("PATCH /api/projects/{project_id}/agent-settings", s.updateAgentSettings)
	mux.HandleFunc("GET /api/conversations", s.listConversations)
	mux.HandleFunc("GET /api/conversations/{conversation_id}", s.getConversation)
	mux.HandleFunc("GET /api/conversations/{conversation_id}/context", s.getConversationContext)
	mux.HandleFunc("DELETE /api/conversations/{conversation_id}", s.deleteConversation)
	mux.HandleFunc("PATCH /api/conversations/{conversation_id}/memory", s.updateConversationMemory)
	mux.HandleFunc("POST /api/query", s.query)
	mux.HandleFunc("POST /api/query/stream", s.queryStream)
	mux.HandleFunc("GET /api/retrieval/trace", s.retrievalTrace)
	ui, uiErr := webui.Handler(s.Settings.WebUIDir)
	if uiErr != nil {
		mux.HandleFunc("GET /static/", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Interfaz no disponible.", http.StatusInternalServerError)
		})
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Interfaz no disponible.", http.StatusInternalServerError)
		})
	} else {
		mux.Handle("GET /static/", ui)
		mux.Handle("GET /{$}", ui)
	}
	return requestIDs(Security(s.runtimeGuard(mux)))
}

func (s *Server) runtimeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/vault/select", "/api/config/chat-profile":
			s.runtimeMu.Lock()
			defer s.runtimeMu.Unlock()
		case "/api/index/start":
			// startIndex transfiere su RLock a la tarea en segundo plano.
		case "/api/vault/pick":
			// El diálogo nativo puede quedar abierto minutos esperando al
			// usuario (o colgado si el proceso no responde). Retener aquí
			// el RLock bloquearía a los escritores todo ese tiempo;
			// pickVault toma su propio RLock breve solo para leer la ruta
			// actual.
		default:
			s.runtimeMu.RLock()
			defer s.runtimeMu.RUnlock()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	store := s.store()
	if store == nil {
		s.internalError(w, "leer el estado", errors.New("SQLite no configurado"))
		return
	}
	stats, err := store.Stats(r.Context())
	if err != nil {
		s.internalError(w, "leer el estado", err)
		return
	}
	graph, err := store.GraphStatus(r.Context())
	if err != nil {
		s.internalError(w, "leer el grafo", err)
		return
	}
	vectorStats := any(map[string]any{})
	contextInfo := resolveModelContext(r.Context(), s)
	if s.Retriever != nil && s.Retriever.Vectors != nil {
		if value, vectorErr := s.Retriever.Vectors.Stats(r.Context()); vectorErr == nil {
			vectorStats = value
		}
	}
	documentStatuses := []map[string]any{}
	rows, rowErr := store.ReadDB().QueryContext(r.Context(), `SELECT lower(COALESCE(json_extract(metadata_json,'$.estado'),json_extract(metadata_json,'$.status'),'')),COUNT(*) FROM documents GROUP BY 1 HAVING 1<>'' ORDER BY 1`)
	if rowErr == nil {
		defer rows.Close()
		for rows.Next() {
			var value string
			var count int
			if rows.Scan(&value, &count) == nil {
				documentStatuses = append(documentStatuses, map[string]any{"value": value, "documents": count})
			}
		}
	}
	graphPayload := map[string]any{"available": graph.Available, "total_edges": graph.TotalEdges, "resolved_edges": graph.ResolvedEdges, "broken_edges": graph.BrokenEdges, "orphan_documents": graph.OrphanDocuments, "ambiguous_aliases": graph.AmbiguousAliases, "configured": s.Settings.GraphSearchEnabled, "active": s.Settings.GraphSearchEnabled && graph.Available, "weight": s.Settings.GraphWeight, "max_hops": s.Settings.GraphMaxHops, "decay": s.Settings.GraphDecay, "backlink_weight": s.Settings.GraphBacklinkWeight, "seed_documents": s.Settings.GraphSeedDocuments, "max_candidates": s.Settings.GraphMaxCandidates, "wiki_link_expansion_enabled": !s.Settings.GraphSearchEnabled}
	ollamaStatus := map[string]any{
		"accessible": false, "base_url": s.Settings.OllamaBaseURL, "version": nil,
		"chat_model":      map[string]any{"name": s.Settings.ChatModel, "installed": nil},
		"embedding_model": map[string]any{"name": s.Settings.EmbeddingModel, "installed": nil},
		"error":           "No se pudo consultar el estado de Ollama local.",
	}
	if inspector, ok := s.Generator.(statusInspector); ok {
		ollamaStatus = inspector.Status(r.Context())
	}
	reranking := rag.RerankAvailability{Backend: "noop", Reason: "Reranking desactivado."}
	if s.Retriever != nil && s.Retriever.Reranker != nil {
		reranking = s.Retriever.Reranker.Availability(r.Context())
	}
	_, vaultErr := os.Stat(s.Settings.VaultPath)
	build := s.Build
	if build.Version == "" {
		build = BuildInfo{Version: "dev", Commit: "unknown", Date: "unknown"}
	}
	watchStatus := map[string]any{"enabled": false, "state": "disabled", "error": nil, "last_indexed_at": nil}
	if s.Watcher != nil {
		watchStatus = s.Watcher.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vault_path": s.Settings.VaultPath, "vault_display_path": s.Settings.VaultPath,
		"vault_name":   filepath.Base(filepath.Clean(s.Settings.VaultPath)),
		"vault_exists": vaultErr == nil, "database_path": s.Settings.DatabasePath,
		"build": build,
		"capabilities": map[string]any{
			"conversation_history": true, "history_pagination": true, "projects": true,
			"conversation_memory": true, "query_rewrite": true, "hybrid_search": true,
			"graph_search": true, "agentic_retrieval": true, "project_agents": true,
			"project_context_usage": true, "project_memory": true,
			"streaming": s.Settings.StreamEnabled, "background_indexing": true,
			"index_cancellation": true, "health_readiness": true, "temporal_validity": true,
			"tag_filters": true, "frontmatter_filters": true, "citation_validation": true,
			"context_inspector": true, "native_vault_picker": s.vaultPickerAvailable(), "reranking": true,
		},
		"operation": map[string]any{
			"history_page_size": s.Settings.HistoryPageSize, "history_max_page_size": s.Settings.HistoryMaxPageSize,
			"read_header_timeout_seconds": s.Settings.HTTPReadHeaderTimeoutSeconds,
			"idle_timeout_seconds":        s.Settings.HTTPIdleTimeoutSeconds,
			"shutdown_timeout_seconds":    s.Settings.HTTPShutdownTimeoutSeconds,
		},
		"memory":        map[string]any{"summary_interval": s.Settings.MemorySummaryInterval, "recent_turns": s.Settings.MemoryRecentTurns, "max_context_chars": s.Settings.MemoryMaxContextChars},
		"chat_profiles": profileList(), "active_chat_profile": activeProfile(s.Settings.ChatModel),
		"model_context": map[string]any{"model": contextInfo.Model, "profile_id": contextInfo.ProfileID, "model_context_window": contextInfo.Window, "source": contextInfo.Source, "verified": contextInfo.Verified, "warning": nilIfEmpty(contextInfo.Warning)},
		"ollama":        ollamaStatus,
		"reranking":     reranking,
		"index":         map[string]any{"stats": stats, "watch": watchStatus}, "retrieval": map[string]any{
			"mode":          map[bool]string{true: "agentic", false: "hybrid"}[s.Settings.AgentMode],
			"agent":         map[string]any{"enabled": s.Settings.AgentMode, "max_iterations": s.Settings.AgentMaxIterations, "token_budget": s.Settings.AgentTokenBudget, "max_subquestions": s.Settings.AgentMaxSubquestions},
			"query_rewrite": map[string]any{"enabled": s.Settings.QueryRewriteEnabled, "context_tokens": s.Settings.QueryRewriteContextTokens, "max_output_tokens": s.Settings.QueryRewriteMaxOutputTokens},
			"read_large":    map[string]any{"enabled": s.Settings.ReadLargeEnabled},
			"hub_boost":     map[string]any{"enabled": s.Settings.HubBoostEnabled, "weight": s.Settings.HubBoostWeight},
			"recency_boost": map[string]any{"enabled": s.Settings.RecencyBoostEnabled, "weight": s.Settings.RecencyBoostWeight},
			"vector_store":  vectorStats, "graph": graphPayload, "rerank": reranking,
		}, "document_statuses": documentStatuses,
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "Solicitud no válida."})
			return
		}
	}
	if err := validateIndexVault(s.Indexer); err != nil {
		s.logErrorContext(r.Context(), "validar el vault antes de indexar", err)
		writeJSON(w, http.StatusConflict, map[string]any{"detail": "La carpeta del vault configurada no existe. Selecciona un vault válido."})
		return
	}
	var result rag.IndexResult
	var err error
	if s.Watcher != nil {
		result, err = s.Watcher.RunFullIndex(r.Context(), nil)
	} else {
		result, err = s.Indexer.Index(r.Context())
	}
	if err != nil {
		s.internalError(w, "indexar el vault", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type queryRequest struct {
	Question       string         `json:"question"`
	TopK           int            `json:"top_k"`
	Status         *string        `json:"status"`
	Vigencia       *string        `json:"vigencia"`
	Tags           []string       `json:"tags"`
	Frontmatter    map[string]any `json:"frontmatter"`
	ExpandLinks    bool           `json:"expand_links"`
	ConversationID *string        `json:"conversation_id"`
	ProjectID      *string        `json:"project_id"`
	UseMemory      bool           `json:"use_memory"`
}

func decodeQuery(r *http.Request) (queryRequest, error) {
	payload := queryRequest{TopK: 6, ExpandLinks: true, UseMemory: true}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return queryRequest{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return queryRequest{}, err
	}
	length := utf8.RuneCountInString(strings.TrimSpace(payload.Question))
	if length < 3 || length > 4000 {
		return queryRequest{}, errors.New("la pregunta debe tener entre 3 y 4000 caracteres")
	}
	if payload.TopK < 1 || payload.TopK > 15 {
		return queryRequest{}, errors.New("top_k debe estar entre 1 y 15")
	}
	if len(payload.Tags) > 10 {
		return queryRequest{}, errors.New("se admiten como máximo 10 tags")
	}
	if err := validateFrontmatterFilters(payload.Frontmatter); err != nil {
		return queryRequest{}, err
	}
	allowed := map[string]bool{"": true, "vigente": true, "futura": true, "caducada": true, "desconocida": true, "no_caducada": true}
	if payload.Vigencia != nil && !allowed[strings.ToLower(strings.TrimSpace(*payload.Vigencia))] {
		return queryRequest{}, errors.New("vigencia no válida")
	}
	return payload, nil
}

type preparedQuery struct {
	payload            queryRequest
	projectID          string
	conversationMemory string
	projectMemory      string
	agentSettings      db.AgentSettings
	semanticQuestion   string
	queryRewrite       map[string]any
	modelContext       modelContextInfo
}

func (s *Server) prepareQuery(ctx context.Context, payload queryRequest) (preparedQuery, error) {
	store := s.store()
	if store == nil {
		return preparedQuery{}, errors.New("SQLite no configurado")
	}
	project := projectID(payload)
	if payload.ConversationID != nil && strings.TrimSpace(*payload.ConversationID) != "" {
		conversationProject, err := store.ConversationProjectID(ctx, *payload.ConversationID)
		if err != nil {
			return preparedQuery{}, err
		}
		project = conversationProject
	} else {
		exists, err := store.ProjectExists(ctx, project)
		if err != nil {
			return preparedQuery{}, err
		}
		if !exists {
			return preparedQuery{}, db.ErrNotFound
		}
	}
	agentSettings, err := store.GetAgentSettings(ctx, project)
	if err != nil {
		return preparedQuery{}, err
	}
	conversationMemory, projectMemory := "", ""
	if s.Memory != nil {
		if payload.UseMemory && payload.ConversationID != nil {
			conversationMemory, err = s.Memory.ConversationContext(ctx, *payload.ConversationID)
			if err != nil {
				return preparedQuery{}, err
			}
		}
		if agentSettings.ProjectMemoryEnabled {
			projectMemory, err = s.Memory.ProjectContext(ctx, project)
			if err != nil {
				return preparedQuery{}, err
			}
		}
	}
	combined := strings.TrimSpace(strings.Join([]string{conversationMemory, projectMemory}, "\n\n"))
	rewritten := rag.QueryRewriteResult{Question: strings.TrimSpace(payload.Question), Metrics: map[string]any{
		"status": "covered_by_agentic_decomposition", "enabled": s.Settings.QueryRewriteEnabled,
		"original_question": strings.TrimSpace(payload.Question), "autonomous_question": strings.TrimSpace(payload.Question),
		"changed": false, "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "estimated": false, "warning": nil,
	}}
	if !s.Settings.AgentMode {
		rewritten = rag.RewriteStandaloneQuestion(ctx, s.Generator, payload.Question, combined, rag.QueryRewriteOptions{
			Enabled: s.Settings.QueryRewriteEnabled, ContextTokens: s.Settings.QueryRewriteContextTokens,
			MaxOutputTokens: s.Settings.QueryRewriteMaxOutputTokens, MaxMemoryChars: s.Settings.QueryRewriteMaxMemoryChars,
		})
	}
	return preparedQuery{payload: payload, projectID: project, conversationMemory: conversationMemory, projectMemory: projectMemory, agentSettings: agentSettings, semanticQuestion: rewritten.Question, queryRewrite: rewritten.Metrics, modelContext: resolveModelContext(ctx, s)}, nil
}

func (s *Server) retrieve(ctx context.Context, prepared preparedQuery, onStage func(string)) (rag.AgenticRetrievalResult, error) {
	if s.Retriever == nil {
		return rag.AgenticRetrievalResult{}, errors.New("recuperador no configurado")
	}
	payload := prepared.payload
	status, vigencia := "", ""
	if payload.Status != nil {
		status = *payload.Status
	}
	if payload.Vigencia != nil {
		vigencia = *payload.Vigencia
	}
	retrievalQuestion := prepared.semanticQuestion
	if s.Settings.AgentMode {
		retrievalQuestion = payload.Question
	}
	request := rag.RetrievalRequest{Question: retrievalQuestion, SemanticQuestion: retrievalQuestion, TopK: payload.TopK, MinSimilarity: s.Settings.MinSimilarity, Status: status, Vigencia: vigencia, Tags: payload.Tags, Frontmatter: payload.Frontmatter, ExpandLinks: payload.ExpandLinks}
	if s.Settings.AgentMode {
		return rag.RunAgenticRetrieval(ctx, s.Generator, s.Retriever, request, rag.AgenticRetrievalOptions{
			MaxIterations: s.Settings.AgentMaxIterations, TokenBudget: s.Settings.AgentTokenBudget,
			MaxSubquestions: s.Settings.AgentMaxSubquestions, ContextTokens: s.Settings.AgentContextTokens,
			MaxObservationChars: s.Settings.AgentMaxObservationChars,
			ConversationMemory:  prepared.conversationMemory, ProjectMemory: prepared.projectMemory, OnStage: onStage,
		})
	}
	retrieval, err := s.Retriever.Retrieve(ctx, request)
	if err != nil {
		return rag.AgenticRetrievalResult{}, err
	}
	return rag.AgenticRetrievalResult{Retrieval: retrieval, Metrics: map[string]any{"status": "disabled", "mode": "deterministic"}}, nil
}

func validateFrontmatterFilters(filters map[string]any) error {
	if len(filters) > 10 {
		return errors.New("se admiten como máximo 10 filtros de frontmatter")
	}
	for key, value := range filters {
		key = strings.TrimSpace(key)
		if key == "" || strings.HasPrefix(key, "_") || utf8.RuneCountInString(key) > 80 {
			return errors.New("clave de frontmatter no válida")
		}
		switch typed := value.(type) {
		case string:
			if utf8.RuneCountInString(typed) > 500 {
				return errors.New("valor de frontmatter demasiado largo")
			}
		case float64, bool, nil:
		case []any:
			if len(typed) > 20 {
				return errors.New("demasiados valores en un filtro de frontmatter")
			}
			for _, item := range typed {
				switch item.(type) {
				case string, float64, bool, nil:
				default:
					return errors.New("valor de frontmatter no válido")
				}
			}
		default:
			return errors.New("valor de frontmatter no válido")
		}
	}
	return nil
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeQuery(r)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	prepared, err := s.prepareQuery(r.Context(), payload)
	if err != nil {
		s.internalQueryError(w, "preparar consulta", err)
		return
	}
	execution, err := s.retrieve(r.Context(), prepared, nil)
	if err != nil {
		s.internalQueryError(w, "recuperar documentos", err)
		return
	}
	retrieval := execution.Retrieval
	s.recordRetrievalTrace(retrieval.Trace)
	sources := buildSources(retrieval.Hits, filepath.Base(filepath.Clean(s.Settings.VaultPath)))
	options := pipelineOptions(s, prepared, nil)
	options.ResearchBrief = execution.ResearchBrief
	pipeline, err := rag.RunAgentPipeline(r.Context(), s.Generator, payload.Question, retrieval.Hits, options)
	if err != nil {
		s.internalQueryError(w, "generar respuesta", err)
		return
	}
	answer := pipeline.Answer
	answer, citations := rag.ValidateAnswer(answer, len(sources))
	agents := pipeline.Metrics
	agents["query_rewrite"] = prepared.queryRewrite
	agents["research"] = execution.Metrics
	agents["rerank"] = retrieval.Rerank
	agents["retrieval_trace"] = retrieval.Trace
	saved, err := s.persistTurn(r.Context(), prepared, answer, sources, agents, len(retrieval.Hits) > 0)
	if err != nil {
		s.internalQueryError(w, "guardar respuesta", err)
		return
	}
	writeJSON(w, http.StatusOK, queryPayload(s, prepared, answer, sources, retrieval, citations, saved, agents))
}

func (s *Server) queryStream(w http.ResponseWriter, r *http.Request) {
	if !s.Settings.StreamEnabled {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": "El streaming está desactivado (RAG_STREAMING=false)."})
		return
	}
	payload, err := decodeQuery(r)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "No se pudo procesar la consulta."})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// Cada consulta en streaming usa su propia conexión y se cierra al
	// terminar. Compartir una conexión keep-alive entre respuestas SSE
	// sucesivas abre la puerta a que un cliente que deje de leer a mitad
	// reciba después la cola de la respuesta anterior (síntoma: "responde
	// a la pregunta anterior") o agote el cupo de conexiones del navegador
	// (síntoma: "network error" en local).
	w.Header().Set("Connection", "close")
	prepared, err := s.prepareQuery(r.Context(), payload)
	if err != nil {
		s.logErrorContext(r.Context(), "preparar consulta", err)
		writeSSE(w, flusher, "error", map[string]any{"detail": "No se pudo procesar la consulta."})
		return
	}
	researchStages := []string{}
	execution, err := s.retrieve(r.Context(), prepared, func(stage string) {
		researchStages = append(researchStages, stage)
	})
	if err != nil {
		s.logErrorContext(r.Context(), "recuperar documentos", err)
		writeSSE(w, flusher, "error", map[string]any{"detail": "No se pudo procesar la consulta."})
		return
	}
	retrieval := execution.Retrieval
	s.recordRetrievalTrace(retrieval.Trace)
	sources := buildSources(retrieval.Hits, filepath.Base(filepath.Clean(s.Settings.VaultPath)))
	if err := writeSSE(w, flusher, "retrieval", map[string]any{"sources": sources, "count": len(sources), "project_id": prepared.projectID, "trace": retrieval.Trace, "query_rewrite": prepared.queryRewrite}); err != nil {
		s.logErrorContext(r.Context(), "enviar fuentes en streaming", err)
		return
	}
	for _, stage := range researchStages {
		if err := writeSSE(w, flusher, "stage", map[string]any{"stage": stage}); err != nil {
			s.logErrorContext(r.Context(), "enviar etapa de búsqueda en streaming", err)
			return
		}
	}
	options := pipelineOptions(s, prepared, func(stage string) {
		_ = writeSSE(w, flusher, "stage", map[string]any{"stage": stage})
	})
	options.ResearchBrief = execution.ResearchBrief
	// Si el navegador cierra la conexión a mitad, la escritura del delta
	// falla y se aborta la generación en curso en lugar de seguir pidiendo
	// tokens a Ollama contra un socket ya muerto.
	pipeline, err := rag.StreamAgentPipeline(r.Context(), s.Generator, payload.Question, retrieval.Hits, options, func(piece string) error {
		return writeSSE(w, flusher, "delta", map[string]any{"text": piece})
	})
	if err != nil {
		s.logErrorContext(r.Context(), "generar respuesta en streaming", err)
		writeSSE(w, flusher, "error", map[string]any{"detail": "No se pudo procesar la consulta."})
		return
	}
	answer := pipeline.Answer
	answer, citations := rag.ValidateAnswer(answer, len(sources))
	agents := pipeline.Metrics
	agents["query_rewrite"] = prepared.queryRewrite
	agents["research"] = execution.Metrics
	agents["rerank"] = retrieval.Rerank
	agents["retrieval_trace"] = retrieval.Trace
	saved, err := s.persistTurn(r.Context(), prepared, answer, sources, agents, len(retrieval.Hits) > 0)
	if err != nil {
		s.logErrorContext(r.Context(), "guardar respuesta", err)
		writeSSE(w, flusher, "error", map[string]any{"detail": "No se pudo guardar la respuesta."})
		return
	}
	writeSSE(w, flusher, "done", queryPayload(s, prepared, answer, sources, retrieval, citations, saved, agents))
}

func queryPayload(s *Server, prepared preparedQuery, answer string, sources []map[string]any, retrieval rag.RetrievalResult, citations rag.CitationReport, saved persistedTurn, agents map[string]any) map[string]any {
	return map[string]any{
		"answer": answer, "sources": sources, "conversation_id": saved.ConversationID,
		"project_id": prepared.projectID, "turn_id": saved.TurnID, "chat_model": s.Settings.ChatModel,
		"memory": saved.Memory, "project_memory": saved.ProjectMemory,
		"agents": agents, "citations": citations,
		"retrieval": map[string]any{"trace": retrieval.Trace, "abstained": retrieval.Abstained, "abstention_reason": retrieval.AbstentionReason, "query_rewrite": prepared.queryRewrite},
	}
}

func (s *Server) recordRetrievalTrace(trace rag.RetrievalTrace) {
	s.traceMu.Lock()
	s.lastTrace = &storedRetrievalTrace{CreatedAt: time.Now().UTC(), Trace: trace}
	s.traceMu.Unlock()
}

func (s *Server) clearRetrievalTrace() {
	s.traceMu.Lock()
	s.lastTrace = nil
	s.traceMu.Unlock()
}

func (s *Server) retrievalTrace(w http.ResponseWriter, _ *http.Request) {
	s.traceMu.RLock()
	trace := s.lastTrace
	s.traceMu.RUnlock()
	if trace == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Todavía no hay una traza de recuperación."})
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func buildSources(hits []map[string]any, vaultNames ...string) []map[string]any {
	vaultName := ""
	if len(vaultNames) > 0 {
		vaultName = vaultNames[0]
	}
	sources := make([]map[string]any, 0, len(hits))
	for index, hit := range hits {
		path, heading := fmt.Sprint(hit["path"]), fmt.Sprint(hit["heading"])
		file := path
		if strings.TrimSpace(heading) != "" {
			file += "#" + heading
		}
		deepLink := url.URL{Scheme: "obsidian", Host: "open", RawQuery: url.Values{"vault": {vaultName}, "file": {file}}.Encode()}
		sources = append(sources, map[string]any{
			"reference": fmt.Sprintf("[%d]", index+1), "title": hit["title"], "path": hit["path"], "heading": hit["heading"],
			"heading_path": hit["heading_path"], "outline": hit["outline"], "mtime": hit["mtime"], "context_expansion": hit["context_expansion"],
			"score": round4(floatAny(hit["score"])), "semantic_score": nullableRound(hit["semantic_score"]), "lexical_score": nullableRound(hit["lexical_score"]), "graph_score": nullableRound(hit["graph_score"]), "fusion_score": nullableRound(hit["fusion_score"]), "mmr_score": nullableRound(hit["mmr_score"]), "rerank_score": nullableRound(hit["rerank_score"]), "rerank_combined_score": nullableRound(hit["rerank_combined_score"]), "backlink_count": hit["backlink_count"], "hub_score": nullableRound(hit["hub_score"]), "hub_boost": nullableRound(hit["hub_boost"]), "recency_score": nullableRound(hit["recency_score"]), "recency_boost": nullableRound(hit["recency_boost"]),
			"reason": hit["reason"], "metadata": hit["metadata"], "content": hit["content"], "matched_content": hit["matched_content"], "context_expanded": hit["context_expanded"],
			"matched_chunk_ids": hit["matched_chunk_ids"], "tags": hit["tags"], "vigencia": hit["vigencia"], "obsidian_url": deepLink.String(),
		})
	}
	return sources
}

func projectID(payload queryRequest) string {
	if payload.ProjectID != nil && strings.TrimSpace(*payload.ProjectID) != "" {
		return *payload.ProjectID
	}
	return "default"
}
func nullableRound(value any) any {
	if value == nil {
		return nil
	}
	return round4(floatAny(value))
}
func floatAny(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	}
	return 0
}
func round4(value float64) float64 { return math.Round(value*10000) / 10000 }
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	data, _ := json.Marshal(payload)
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *Server) internalError(w http.ResponseWriter, action string, err error) {
	s.writeClassifiedError(w, action, err, "No se pudo completar la operación.")
}
func (s *Server) internalQueryError(w http.ResponseWriter, action string, err error) {
	s.writeClassifiedError(w, action, err, "No se pudo procesar la consulta.")
}

func (s *Server) writeClassifiedError(w http.ResponseWriter, action string, err error, message string) {
	status := httpStatusForError(err)
	requestID := requestIDFromResponse(w)
	s.logErrorWithID(requestID, action, err)
	payload := map[string]any{"detail": message}
	if requestID != "" {
		payload["request_id"] = requestID
	}
	writeJSON(w, status, payload)
}

func httpStatusForError(err error) int {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, db.ErrDuplicate), errors.Is(err, rag.ErrIndexing):
		return http.StatusConflict
	case errors.Is(err, ollama.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) logError(action string, err error) {
	s.logErrorWithID("", action, err)
}

func (s *Server) logErrorContext(ctx context.Context, action string, err error) {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	s.logErrorWithID(requestID, action, err)
}

func (s *Server) logErrorWithID(requestID, action string, err error) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("request failed", "request_id", requestID, "action", action, "error", err)
}

func NewHTTPServer(address string, handler http.Handler, settings config.Settings) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: time.Duration(max(1, settings.HTTPReadHeaderTimeoutSeconds)) * time.Second,
		IdleTimeout:       time.Duration(max(10, settings.HTTPIdleTimeoutSeconds)) * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func Listen(ctx context.Context, address string, handler http.Handler, logger *slog.Logger, settings config.Settings) error {
	server := NewHTTPServer(address, handler, settings)
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Duration(max(1, settings.HTTPShutdownTimeoutSeconds))*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil && logger != nil {
			logger.Error("server graceful shutdown failed", "error", err)
			_ = server.Close()
		}
	}()
	if logger != nil {
		logger.Info("server listening", "url", "http://"+address)
	}
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{"sqlite": false, "ollama": nil}
	ready := true
	store := s.store()
	if store == nil || store.ReadDB().PingContext(r.Context()) != nil {
		ready = false
	} else {
		checks["sqlite"] = true
	}
	if inspector, ok := s.Generator.(statusInspector); ok {
		statusContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		status := inspector.Status(statusContext)
		cancel()
		accessible, _ := status["accessible"].(bool)
		checks["ollama"] = accessible
		ready = ready && accessible
	}
	code, state := http.StatusOK, "ready"
	if !ready {
		code, state = http.StatusServiceUnavailable, "not_ready"
	}
	writeJSON(w, code, map[string]any{"status": state, "checks": checks})
}

type requestIDKey struct{}

func requestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, 12)
		if _, err := rand.Read(data); err != nil {
			copy(data, []byte(fmt.Sprint(time.Now().UnixNano())))
		}
		requestID := hex.EncodeToString(data)
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromResponse(w http.ResponseWriter) string { return w.Header().Get("X-Request-ID") }
