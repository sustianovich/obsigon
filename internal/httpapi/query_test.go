package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

type queryEmbedder struct {
	vector []float32
	seen   *[]string
}

func (e queryEmbedder) Dimension(context.Context) (int, error) { return len(e.vector), nil }
func (e queryEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.seen != nil {
		*e.seen = append(*e.seen, texts...)
	}
	result := make([][]float32, len(texts))
	for index := range texts {
		result[index] = append([]float32(nil), e.vector...)
	}
	return result, nil
}

type recordingGenerator struct {
	chatCalls        int
	verifierCalls    int
	streamCalls      int
	lastChatOptions  ollama.ChatOptions
	lastStreamOption ollama.ChatOptions
	modelInfo        map[string]any
	verifierContent  string
	rewriteContent   string
}

type agentHTTPGenerator struct {
	calls            int
	writerSawBrief   bool
	verifierSawBrief bool
}

func (g *agentHTTPGenerator) Chat(_ context.Context, instructions, prompt string, _ ollama.ChatOptions) (ollama.GenerationResult, error) {
	g.calls++
	switch instructions {
	case rag.AgentDecompositionInstructions:
		return ollama.GenerationResult{Content: `{"subquestions":["¿Qué exige la validación?","¿Cuántas comprobaciones requiere?"]}`, PromptTokens: 12, CompletionTokens: 8}, nil
	case rag.AgentResearchInstructions:
		return ollama.GenerationResult{Content: `{"tool":"finish","arguments":{"answer":"La evidencia indica dos comprobaciones."}}`, PromptTokens: 20, CompletionTokens: 9}, nil
	case rag.VerifierInstructions:
		g.verifierSawBrief = strings.Contains(prompt, "BORRADOR DEL AGENTE DE BÚSQUEDA")
		return ollama.GenerationResult{Content: "SUFICIENCIA: suficiente\n\nLa fuente [1] es pertinente.", PromptTokens: 10, CompletionTokens: 5}, nil
	default:
		g.writerSawBrief = strings.Contains(prompt, "BORRADOR DEL AGENTE DE BÚSQUEDA")
		return ollama.GenerationResult{Content: "La validación exige dos comprobaciones [1].", PromptTokens: 20, CompletionTokens: 8}, nil
	}
}

func (g *agentHTTPGenerator) StreamChat(_ context.Context, _ string, prompt string, _ ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	g.writerSawBrief = strings.Contains(prompt, "BORRADOR DEL AGENTE DE BÚSQUEDA")
	answer := "La validación exige dos comprobaciones [1]."
	if err := onDelta(answer); err != nil {
		return ollama.GenerationResult{}, err
	}
	return ollama.GenerationResult{Content: answer, PromptTokens: 20, CompletionTokens: 8}, nil
}

type failingMemoryGenerator struct{}

func (failingMemoryGenerator) Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error) {
	return ollama.GenerationResult{}, errors.New("fallo simulado del resumen")
}

func (g *recordingGenerator) Chat(_ context.Context, instructions, _ string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	g.chatCalls++
	g.lastChatOptions = options
	if instructions == rag.VerifierInstructions {
		g.verifierCalls++
		content := g.verifierContent
		if content == "" {
			content = "SUFICIENCIA: suficiente\n\nLa fuente [1] es pertinente."
		}
		return ollama.GenerationResult{Content: content, PromptTokens: 10, CompletionTokens: 5}, nil
	}
	if instructions == rag.QueryRewriteInstructions {
		content := g.rewriteContent
		if content == "" {
			content = `{"question":"¿Qué exige la validación?"}`
		}
		return ollama.GenerationResult{Content: content, PromptTokens: 8, CompletionTokens: 4}, nil
	}
	return ollama.GenerationResult{Content: "La validación exige dos comprobaciones [1].", PromptTokens: 20, CompletionTokens: 8}, nil
}
func (g *recordingGenerator) StreamChat(_ context.Context, _, _ string, options ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	g.streamCalls++
	g.lastStreamOption = options
	for _, piece := range []string{"La validación ", "exige dos comprobaciones [1]."} {
		if err := onDelta(piece); err != nil {
			return ollama.GenerationResult{}, err
		}
	}
	return ollama.GenerationResult{Content: "La validación exige dos comprobaciones [1].", PromptTokens: 20, CompletionTokens: 8}, nil
}

func (g *recordingGenerator) ModelShow(context.Context, string) (map[string]any, error) {
	if g.modelInfo == nil {
		return nil, errors.New("metadatos no disponibles")
	}
	return map[string]any{"model_info": g.modelInfo}, nil
}

func queryServerFixture(t *testing.T, minSimilarity float64) (*Server, *recordingGenerator) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "query.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ReplaceDocument(context.Background(), db.Document{
		Path: "protocolos/validacion.md", Title: "Protocolo de validación", SHA256: "fixture",
		Metadata: map[string]any{"estado": "aprobado", "tipo": "protocolo", "aliases": []string{"PV"}, "_tags": []string{"protocolo/clinico"}},
		Chunks:   []db.Chunk{{Index: 0, Heading: "Criterios", Content: "La validación exige dos comprobaciones críticas.", Embedding: []float32{1, 0, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		ChatModel: "qwen-local", EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		TopK: 6, MinSimilarity: minSimilarity, MinRelativeScore: .62, MaxOutputTokens: 256,
		HybridSearchEnabled: true, HybridRRFK: 60, HybridSemanticWeight: 1, HybridLexicalWeight: .8,
		HybridCandidateMultiplier: 4, MMREnabled: true, MMRLambda: .72, MaxChunksPerDocument: 2,
		EmbeddingWorkers: 2, StreamEnabled: true,
	}
	vectors := rag.NewVectorStore(store, 2)
	retriever := &rag.Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: queryEmbedder{vector: []float32{1, 0, 0}}, Reranker: rag.NewNoopReranker()}
	generator := &recordingGenerator{}
	return &Server{Settings: settings, Retriever: retriever, Generator: generator}, generator
}

func performQuery(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(`{"question":"¿Qué exige la validación?","top_k":3}`))
	request.Host = "127.0.0.1"
	request.Header.Set("Origin", "http://127.0.0.1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func performQueryBody(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body))
	request.Host = "127.0.0.1"
	request.Header.Set("Origin", "http://127.0.0.1")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("respuesta JSON no válida: %v: %s", err, response.Body.String())
	}
	return payload
}

func TestQueryReturnsCitedSources(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK || generator.chatCalls != 2 || generator.verifierCalls != 1 {
		t.Fatalf("status=%d llamadas=%d verificador=%d cuerpo=%s", response.Code, generator.chatCalls, generator.verifierCalls, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{`"reference":"[1]"`, `"path":"protocolos/validacion.md"`, `"valid":1`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("falta %s en %s", expected, body)
		}
	}
}

func TestQueryAcceptsFrontmatterAndNestedTagFilters(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	response := performQueryBody(t, server.Handler(), "/api/query", `{"question":"¿Qué exige la validación?","top_k":3,"tags":["protocolo"],"frontmatter":{"tipo":"protocolo","aliases":"PV"}}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "protocolos/validacion.md") {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	invalid := performQueryBody(t, server.Handler(), "/api/query", `{"question":"¿Qué exige la validación?","top_k":3,"frontmatter":{"_tags":"privado"}}`)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("se aceptó un filtro interno: status=%d cuerpo=%s", invalid.Code, invalid.Body.String())
	}
}

func TestQueryRejectsUnknownAndTrailingJSONFields(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	for _, body := range []string{
		`{"question":"¿Qué exige la validación?","desconocido":true}`,
		`{"question":"¿Qué exige la validación?"} {"extra":true}`,
	} {
		response := performQueryBody(t, server.Handler(), "/api/query", body)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s status=%d respuesta=%s", body, response.Code, response.Body.String())
		}
	}
}

func TestQueryCanUseOptInAgenticRetrieval(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	generator := &agentHTTPGenerator{}
	server.Generator = generator
	server.Settings.AgentMode = true
	server.Settings.AgentMaxIterations = 4
	server.Settings.AgentTokenBudget = 1200
	server.Settings.AgentMaxSubquestions = 3
	server.Settings.AgentContextTokens = 8192
	server.Settings.AgentMaxObservationChars = 8000
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	payload := responseObject(t, response)
	agents := payload["agents"].(map[string]any)
	research := agents["research"].(map[string]any)
	retrieval := payload["retrieval"].(map[string]any)
	trace := retrieval["trace"].(map[string]any)
	if research["mode"] != "agentic" || research["stop_reason"] != "finish" || len(research["subquestions"].([]any)) != 2 || trace["version"] != "2-agentic" {
		t.Fatalf("research=%+v trace=%+v", research, trace)
	}
	if generator.calls != 4 || !generator.verifierSawBrief || !generator.writerSawBrief {
		t.Fatalf("generator=%+v", generator)
	}
}

func TestFollowUpEmbedsOnlyAutonomousRewrite(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	server.Settings.QueryRewriteEnabled = true
	server.Settings.QueryRewriteContextTokens = 4096
	server.Settings.QueryRewriteMaxOutputTokens = 120
	server.Settings.QueryRewriteMaxMemoryChars = 4000
	server.Settings.MemoryRecentTurns = 2
	server.Settings.MemoryMaxContextChars = 6000
	server.Settings.MemorySummaryInterval = 10
	server.Settings.MemorySummaryMaxInputChars = 8000
	server.Settings.MemorySummaryMaxTokens = 200
	server.Settings.ProjectMemorySummaryInterval = 10
	server.Settings.ProjectMemorySummaryMaxInputChars = 8000
	server.Settings.ProjectMemorySummaryMaxTokens = 200
	server.Memory = &rag.MemoryManager{Settings: server.Settings, Store: server.store(), Generator: generator}
	seen := []string{}
	server.Retriever.Embedder = queryEmbedder{vector: []float32{1, 0, 0}, seen: &seen}
	first := performQuery(t, server.Handler(), "/api/query")
	if first.Code != http.StatusOK {
		t.Fatalf("primer turno: %s", first.Body.String())
	}
	id := responseObject(t, first)["conversation_id"].(string)
	generator.rewriteContent = `{"question":"¿Cuáles son las excepciones del protocolo de validación?"}`
	body, _ := json.Marshal(map[string]any{"question": "¿y sus excepciones?", "top_k": 3, "conversation_id": id})
	second := performQueryBody(t, server.Handler(), "/api/query", string(body))
	if second.Code != http.StatusOK {
		t.Fatalf("segundo turno: %s", second.Body.String())
	}
	if len(seen) < 2 {
		t.Fatalf("embeddings=%v", seen)
	}
	last := seen[len(seen)-1]
	if !strings.Contains(last, "excepciones del protocolo de validación") || strings.Contains(last, "TURNOS RECIENTES") || strings.Contains(last, "dos comprobaciones") {
		t.Fatalf("el embedding contiene memoria cruda o no usa la reformulación: %q", last)
	}
	payload := responseObject(t, second)
	rewrite := payload["retrieval"].(map[string]any)["query_rewrite"].(map[string]any)
	if rewrite["status"] != "completed" || rewrite["changed"] != true {
		t.Fatalf("rewrite=%+v", rewrite)
	}
}

func TestAgenticStreamingPreservesRetrievalFirstContract(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	generator := &agentHTTPGenerator{}
	server.Generator = generator
	server.Settings.AgentMode = true
	server.Settings.AgentMaxIterations = 4
	server.Settings.AgentTokenBudget = 1200
	server.Settings.AgentMaxSubquestions = 3
	server.Settings.AgentContextTokens = 8192
	server.Settings.AgentMaxObservationChars = 8000
	response := performQuery(t, server.Handler(), "/api/query/stream")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	text := response.Body.String()
	events := []string{"event: retrieval", `"stage":"decomposition"`, `"stage":"research"`, `"stage":"verifier"`, `"stage":"writer"`, "event: delta", "event: done"}
	previous := -1
	for _, event := range events {
		position := strings.Index(text, event)
		if position <= previous {
			t.Fatalf("orden SSE agéntico incorrecto en %s: %s", event, text)
		}
		previous = position
	}
	if !generator.verifierSawBrief || !generator.writerSawBrief {
		t.Fatalf("el borrador no llegó a verificador/redactor: %+v", generator)
	}
}

func TestQueryPersistsTurnAndReportsContextMetrics(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	payload := responseObject(t, response)
	conversationID, _ := payload["conversation_id"].(string)
	if conversationID == "" || payload["turn_id"].(float64) < 1 {
		t.Fatalf("turno no persistido: %+v", payload)
	}
	conversation, err := server.store().GetConversation(context.Background(), conversationID)
	if err != nil {
		t.Fatal(err)
	}
	turns, _ := conversation["turns"].([]map[string]any)
	if len(turns) != 1 || turns[0]["answer"] != payload["answer"] {
		t.Fatalf("conversación=%+v", conversation)
	}
	agents, _ := payload["agents"].(map[string]any)
	writer, _ := agents["writer"].(map[string]any)
	if writer["status"] != "completed" || writer["effective_context_window"].(float64) < 4096 || writer["total_tokens"].(float64) != 28 {
		t.Fatalf("telemetría del redactor=%+v", writer)
	}
	if generator.lastChatOptions.ContextWindow != 16384 {
		t.Fatalf("num_ctx=%d", generator.lastChatOptions.ContextWindow)
	}
}

func TestQueryUsesConservativeModelContext(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	server.Settings.ChatModel = "qwen3.5:4b"
	server.Settings.WriterContextTokens = 40000
	generator.modelInfo = map[string]any{"qwen2.context_length": float64(8192), "otra.context_length": float64(16384)}
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	payload := responseObject(t, response)
	agents := payload["agents"].(map[string]any)
	modelContext := agents["model_context"].(map[string]any)
	if generator.lastChatOptions.ContextWindow != 8192 || modelContext["model_context_window"].(float64) != 8192 || modelContext["source"] != "profile+ollama" || modelContext["warning"] == nil {
		t.Fatalf("opciones=%+v contexto=%+v", generator.lastChatOptions, modelContext)
	}
}

func TestMemorySummaryFailureKeepsSavedTurn(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	server.Settings.MemorySummaryInterval = 2
	server.Settings.ProjectMemorySummaryInterval = 2
	server.Settings.MemoryRecentTurns = 4
	server.Settings.MemoryMaxContextChars = 4000
	server.Settings.ProjectMemoryMaxContextChars = 2000
	server.Settings.MemorySummaryMaxInputChars = 8000
	server.Settings.ProjectMemorySummaryMaxInputChars = 8000
	server.Settings.MemorySummaryMaxTokens = 200
	server.Settings.ProjectMemorySummaryMaxTokens = 200
	server.Memory = &rag.MemoryManager{Settings: server.Settings, Store: server.store(), Generator: failingMemoryGenerator{}}

	first := performQuery(t, server.Handler(), "/api/query")
	if first.Code != http.StatusOK {
		t.Fatalf("primer turno: status=%d cuerpo=%s", first.Code, first.Body.String())
	}
	id := responseObject(t, first)["conversation_id"].(string)
	secondBody, _ := json.Marshal(map[string]any{"question": "¿Qué exige de nuevo la validación?", "top_k": 3, "conversation_id": id})
	second := performQueryBody(t, server.Handler(), "/api/query", string(secondBody))
	if second.Code != http.StatusOK {
		t.Fatalf("segundo turno: status=%d cuerpo=%s", second.Code, second.Body.String())
	}
	// El resumen se actualiza en segundo plano; hay que esperarlo antes de
	// comprobar el resultado del fallo tolerado.
	server.Memory.WaitForBackgroundSummaries()

	conversationStatus, err := server.store().ConversationMemoryStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	projectID := responseObject(t, second)["project_id"].(string)
	projectStatus, err := server.store().ProjectMemoryStatus(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if conversationStatus["last_error"] == nil || projectStatus["last_error"] == nil {
		t.Fatalf("no se registró el fallo tolerado: conversación=%+v proyecto=%+v", conversationStatus, projectStatus)
	}
	conversation, err := server.store().GetConversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if turns := conversation["turns"].([]map[string]any); len(turns) != 2 {
		t.Fatalf("se perdió el turno al fallar el resumen: %+v", turns)
	}
}

func TestStreamPublishesRetrievalBeforeVisibleAnswer(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	response := performQuery(t, server.Handler(), "/api/query/stream")
	if response.Code != http.StatusOK || generator.streamCalls != 1 {
		t.Fatalf("status=%d llamadas=%d cuerpo=%s", response.Code, generator.streamCalls, response.Body.String())
	}
	body, _ := io.ReadAll(response.Result().Body)
	text := string(body)
	retrievalAt, verifierAt := strings.Index(text, "event: retrieval"), strings.Index(text, `"stage":"verifier"`)
	writerAt, deltaAt, doneAt := strings.Index(text, `"stage":"writer"`), strings.Index(text, "event: delta"), strings.Index(text, "event: done")
	if retrievalAt < 0 || verifierAt <= retrievalAt || writerAt <= verifierAt || deltaAt <= writerAt || doneAt <= deltaAt {
		t.Fatalf("orden SSE incorrecto: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "<think>") || !strings.Contains(text, "protocolos/validacion.md") {
		t.Fatalf("stream inseguro o sin fuentes: %s", text)
	}
}

func TestVerifierInsufficientCanSkipWriterOverHTTP(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	server.Settings.VerifierAbstainOnInsufficient = true
	generator.verifierContent = "SUFICIENCIA: insuficiente\n\nFalta evidencia concluyente."
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK || generator.verifierCalls != 1 || generator.chatCalls != 1 {
		t.Fatalf("status=%d generador=%+v cuerpo=%s", response.Code, generator, response.Body.String())
	}
	payload := responseObject(t, response)
	agents := payload["agents"].(map[string]any)
	writer := agents["writer"].(map[string]any)
	if writer["status"] != "skipped_insufficient_evidence" || agents["warning"] == nil {
		t.Fatalf("agentes=%+v", agents)
	}
}

func TestHardAbstentionNeverCallsGenerator(t *testing.T) {
	server, generator := queryServerFixture(t, 1.1)
	response := performQuery(t, server.Handler(), "/api/query")
	if response.Code != http.StatusOK || generator.chatCalls != 0 || generator.streamCalls != 0 {
		t.Fatalf("status=%d generador=%+v cuerpo=%s", response.Code, generator, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "skipped_insufficient_evidence") || !strings.Contains(response.Body.String(), `"sources":[]`) {
		t.Fatalf("abstención incompleta: %s", response.Body.String())
	}
}
