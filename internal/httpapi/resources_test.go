package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/rag"
)

func performAPI(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	request.Host = "127.0.0.1"
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", "http://127.0.0.1")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func apiObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("JSON no válido: %v: %s", err, response.Body.String())
	}
	return payload
}

func TestRuntimeGuardWaitsForActiveReadersBeforeMutation(t *testing.T) {
	server := &Server{}
	readerStarted := make(chan struct{})
	releaseReader := make(chan struct{})
	readerDone := make(chan struct{})
	writerDone := make(chan struct{})
	handler := server.runtimeGuard(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/query" {
			close(readerStarted)
			<-releaseReader
			close(readerDone)
			return
		}
		close(writerDone)
	}))

	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/query", nil))
	<-readerStarted
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/vault/select", nil))
	select {
	case <-writerDone:
		t.Fatal("la mutación no esperó a la consulta activa")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseReader)
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("la consulta no terminó")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("la mutación no continuó tras terminar la consulta")
	}
}

func TestProjectAndConversationHTTPFlow(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	server.Settings.VerifierContextTokens = 8192
	server.Settings.WriterContextTokens = 16384
	server.Settings.MemoryRecentTurns = 4
	server.Settings.MemorySummaryInterval = 10
	server.Settings.ProjectMemorySummaryInterval = 10
	server.Settings.MemoryMaxContextChars = 12000
	server.Settings.ProjectMemoryMaxContextChars = 6000
	server.Settings.MaxOutputTokens = 1200
	server.Memory = &rag.MemoryManager{Settings: server.Settings, Store: server.store()}
	handler := server.Handler()

	projects := performAPI(t, handler, http.MethodGet, "/api/projects", "")
	if projects.Code != http.StatusOK || !strings.Contains(projects.Body.String(), `"default_project_id":"default"`) {
		t.Fatalf("proyectos iniciales: status=%d cuerpo=%s", projects.Code, projects.Body.String())
	}
	created := performAPI(t, handler, http.MethodPost, "/api/projects", `{"name":"Auditoría"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("crear: status=%d cuerpo=%s", created.Code, created.Body.String())
	}
	projectID := apiObject(t, created)["id"].(string)

	renamed := performAPI(t, handler, http.MethodPatch, "/api/projects/"+projectID, `{"name":"Auditoría clínica"}`)
	if renamed.Code != http.StatusOK || apiObject(t, renamed)["name"] != "Auditoría clínica" {
		t.Fatalf("renombrar: status=%d cuerpo=%s", renamed.Code, renamed.Body.String())
	}
	settings := performAPI(t, handler, http.MethodPatch, "/api/projects/"+projectID+"/agent-settings", `{"verification_enabled":false,"project_memory_enabled":true,"verifier_context_tokens":4096,"writer_context_tokens":8192}`)
	if settings.Code != http.StatusOK {
		t.Fatalf("ajustes: status=%d cuerpo=%s", settings.Code, settings.Body.String())
	}

	queryBody, _ := json.Marshal(map[string]any{"question": "¿Qué exige la validación?", "top_k": 3, "project_id": projectID})
	query := performAPI(t, handler, http.MethodPost, "/api/query", string(queryBody))
	if query.Code != http.StatusOK {
		t.Fatalf("consulta: status=%d cuerpo=%s", query.Code, query.Body.String())
	}
	queryPayload := apiObject(t, query)
	conversationID := queryPayload["conversation_id"].(string)
	if queryPayload["project_id"] != projectID {
		t.Fatalf("proyecto de la consulta=%+v", queryPayload)
	}

	listed := performAPI(t, handler, http.MethodGet, "/api/conversations?project_id="+projectID, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), conversationID) {
		t.Fatalf("listar conversaciones: status=%d cuerpo=%s", listed.Code, listed.Body.String())
	}
	detail := performAPI(t, handler, http.MethodGet, "/api/conversations/"+conversationID, "")
	if detail.Code != http.StatusOK || len(apiObject(t, detail)["turns"].([]any)) != 1 {
		t.Fatalf("detalle: status=%d cuerpo=%s", detail.Code, detail.Body.String())
	}
	contextResponse := performAPI(t, handler, http.MethodGet, "/api/conversations/"+conversationID+"/context", "")
	if contextResponse.Code != http.StatusOK {
		t.Fatalf("contexto: status=%d cuerpo=%s", contextResponse.Code, contextResponse.Body.String())
	}
	overview := apiObject(t, contextResponse)
	if overview["conversation_id"] != conversationID || overview["history"].(map[string]any)["total_turns"].(float64) != 1 {
		t.Fatalf("inspector=%+v", overview)
	}

	memory := performAPI(t, handler, http.MethodPatch, "/api/conversations/"+conversationID+"/memory", `{"enabled":false}`)
	if memory.Code != http.StatusOK || apiObject(t, memory)["memory"].(map[string]any)["enabled"] != false {
		t.Fatalf("memoria: status=%d cuerpo=%s", memory.Code, memory.Body.String())
	}
	deletedConversation := performAPI(t, handler, http.MethodDelete, "/api/conversations/"+conversationID, "")
	if deletedConversation.Code != http.StatusOK {
		t.Fatalf("eliminar conversación: status=%d cuerpo=%s", deletedConversation.Code, deletedConversation.Body.String())
	}
	if exists, err := server.store().ConversationExists(context.Background(), conversationID); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	deletedProject := performAPI(t, handler, http.MethodDelete, "/api/projects/"+projectID, "")
	if deletedProject.Code != http.StatusOK {
		t.Fatalf("eliminar proyecto: status=%d cuerpo=%s", deletedProject.Code, deletedProject.Body.String())
	}
}

func TestConversationContextReturnsNotFound(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	server.Memory = &rag.MemoryManager{Settings: server.Settings, Store: server.store()}
	response := performAPI(t, server.Handler(), http.MethodGet, "/api/conversations/no-existe/context", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
}

func TestStatusExposesResolvedModelContext(t *testing.T) {
	server, generator := queryServerFixture(t, .3)
	server.Settings.ChatModel = "llama3:latest"
	generator.modelInfo = map[string]any{"llama.context_length": float64(4096)}
	response := performAPI(t, server.Handler(), http.MethodGet, "/api/status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
	modelContext := apiObject(t, response)["model_context"].(map[string]any)
	if modelContext["model_context_window"].(float64) != 4096 || modelContext["source"] != "profile+ollama" || modelContext["verified"] != true {
		t.Fatalf("contexto=%+v", modelContext)
	}
}

func TestNativeVaultPickerContract(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	current := filepath.Join(t.TempDir(), "actual")
	target := filepath.Join(t.TempDir(), "elegido")
	server.Settings.VaultPath = current
	var receivedCurrent string
	server.VaultPicker = func(_ context.Context, initial string) (string, bool, error) {
		receivedCurrent = initial
		return target, true, nil
	}

	handler := server.Handler()
	status := performAPI(t, handler, http.MethodGet, "/api/status", "")
	capabilities := apiObject(t, status)["capabilities"].(map[string]any)
	if capabilities["native_vault_picker"] != true {
		t.Fatalf("capabilities=%+v", capabilities)
	}

	picked := performAPI(t, handler, http.MethodPost, "/api/vault/pick", `{}`)
	if picked.Code != http.StatusOK {
		t.Fatalf("selector: status=%d cuerpo=%s", picked.Code, picked.Body.String())
	}
	payload := apiObject(t, picked)
	if payload["selected"] != true || payload["vault_path"] != filepath.Clean(target) {
		t.Fatalf("selector=%+v", payload)
	}
	if receivedCurrent != current {
		t.Fatalf("ruta inicial=%q, se esperaba %q", receivedCurrent, current)
	}
}

func TestChatProfileAndVaultSelectionContracts(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.Mkdir(vault, 0700); err != nil {
		t.Fatal(err)
	}
	server.Settings.BaseDir = base
	server.Settings.VaultPath = vault
	handler := server.Handler()
	profile := performAPI(t, handler, http.MethodPost, "/api/config/chat-profile", `{"profile":"llama3"}`)
	if profile.Code != http.StatusOK || apiObject(t, profile)["chat_model"] != "llama3:latest" {
		t.Fatalf("perfil: status=%d cuerpo=%s", profile.Code, profile.Body.String())
	}
	envData, err := os.ReadFile(filepath.Join(base, ".env"))
	if err != nil || !strings.Contains(string(envData), "OLLAMA_CHAT_MODEL=llama3:latest") {
		t.Fatalf("env=%q err=%v", envData, err)
	}
	empty := performAPI(t, handler, http.MethodPost, "/api/vault/select", `{}`)
	if empty.Code != http.StatusOK {
		t.Fatalf("vault vacío: status=%d cuerpo=%s", empty.Code, empty.Body.String())
	}
	if payload := apiObject(t, empty); payload["selected"] != false || payload["reason"] != "empty" || payload["vault_exists"] != true {
		t.Fatalf("vault vacío=%+v", payload)
	}

	missing := performAPI(t, handler, http.MethodPost, "/api/vault/select", `{"vault_path":"`+filepath.ToSlash(filepath.Join(base, "no-existe"))+`"}`)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("vault inexistente: status=%d cuerpo=%s", missing.Code, missing.Body.String())
	}

	target := filepath.Join(base, "otro-vault")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store := server.store(); store != nil {
			_ = store.Close()
		}
	})
	previousStore := server.store()
	previousRetriever := server.Retriever
	switched := performAPI(t, handler, http.MethodPost, "/api/vault/select", `{"vault_path":"`+filepath.ToSlash(target)+`"}`)
	if switched.Code != http.StatusOK {
		t.Fatalf("cambio de vault: status=%d cuerpo=%s", switched.Code, switched.Body.String())
	}
	payload := apiObject(t, switched)
	if payload["selected"] != true || payload["reindex_required"] != true {
		t.Fatalf("cambio de vault=%+v", payload)
	}
	if server.Settings.VaultPath != target {
		t.Fatalf("Settings.VaultPath=%q, se esperaba %q", server.Settings.VaultPath, target)
	}
	if server.Retriever.Store != server.store() || server.store() == nil {
		t.Fatal("el store del recuperador no se reenganchó")
	}
	if server.Retriever == previousRetriever {
		t.Fatal("el cambio de vault mutó el recuperador anterior en vez de publicar uno nuevo")
	}
	if err := previousStore.DB.PingContext(context.Background()); err == nil {
		t.Fatal("el store anterior seguía abierto después del cambio serializado")
	}
	envData, err = os.ReadFile(filepath.Join(base, ".env"))
	if err != nil || !strings.Contains(string(envData), "OBSIDIAN_VAULT_PATH="+filepath.ToSlash(target)) {
		t.Fatalf("env=%q err=%v", envData, err)
	}
}

func TestIndexRoutesRunSynchronouslyAndInBackground(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.Mkdir(vault, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "nota.md"), []byte("# Nota\n\nContenido indexable."), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(base, "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := config.Settings{BaseDir: base, VaultPath: vault, DatabasePath: filepath.Join(base, "index.sqlite3"), EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto", ChunkSize: 1800, ChunkOverlap: 250, EmbeddingWorkers: 1}
	indexer := &rag.Indexer{Settings: settings, Store: store, Embedder: queryEmbedder{vector: []float32{1, 0, 0}}}
	server := &Server{Settings: settings, Store: store, Indexer: indexer, Progress: NewIndexProgress()}
	handler := server.Handler()

	synchronous := performAPI(t, handler, http.MethodPost, "/api/index", `{}`)
	if synchronous.Code != http.StatusOK || apiObject(t, synchronous)["scanned"].(float64) != 1 {
		t.Fatalf("index: status=%d cuerpo=%s", synchronous.Code, synchronous.Body.String())
	}
	started := performAPI(t, handler, http.MethodPost, "/api/index/start", `{}`)
	if started.Code != http.StatusOK {
		t.Fatalf("inicio: status=%d cuerpo=%s", started.Code, started.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		progress := performAPI(t, handler, http.MethodGet, "/api/index/progress", "")
		payload := apiObject(t, progress)
		if payload["running"] == false {
			if payload["phase"] != "done" || payload["result"] == nil {
				t.Fatalf("progreso=%+v", payload)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("la indexación no terminó: %+v", payload)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIndexStartRejectsMissingConfiguredVault(t *testing.T) {
	base := t.TempDir()
	store, err := db.Open(filepath.Join(base, "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := config.Settings{
		VaultPath: filepath.Join(base, "no-existe"), EmbeddingModel: "nomic-embed-text",
		EmbeddingPrefixScheme: "auto", ChunkSize: 1800, ChunkOverlap: 250,
	}
	server := &Server{
		Settings: settings,
		Indexer:  &rag.Indexer{Settings: settings, Store: store, Embedder: queryEmbedder{vector: []float32{1, 0, 0}}},
		Progress: NewIndexProgress(),
	}
	response := performAPI(t, server.Handler(), http.MethodPost, "/api/index/start", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "Selecciona un vault válido") {
		t.Fatalf("status=%d cuerpo=%s", response.Code, response.Body.String())
	}
}

func TestSelectedVaultCanBeIndexedImmediately(t *testing.T) {
	base := t.TempDir()
	initialVault := filepath.Join(base, "inicial")
	targetVault := filepath.Join(base, "nuevo-vault")
	for _, path := range []string{initialVault, targetVault} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(targetVault, "nota.md"), []byte("# Nota nueva\n\nContenido indexable."), 0o600); err != nil {
		t.Fatal(err)
	}
	initialDatabase := config.DefaultDatabasePath(base, initialVault)
	initialStore, err := db.Open(initialDatabase)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		BaseDir: base, VaultPath: initialVault, DatabasePath: initialDatabase,
		EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		ChunkSize: 1800, ChunkOverlap: 250, EmbeddingWorkers: 1,
	}
	indexer := &rag.Indexer{Settings: settings, Store: initialStore, Embedder: queryEmbedder{vector: []float32{1, 0, 0}}}
	server := &Server{Settings: settings, Store: initialStore, Indexer: indexer, Progress: NewIndexProgress()}
	handler := server.Handler()

	selected := performAPI(t, handler, http.MethodPost, "/api/vault/select", `{"vault_path":"`+filepath.ToSlash(targetVault)+`"}`)
	if selected.Code != http.StatusOK || apiObject(t, selected)["selected"] != true {
		t.Fatalf("selección: status=%d cuerpo=%s", selected.Code, selected.Body.String())
	}
	if server.Indexer.Settings.VaultPath != targetVault || server.Indexer.Store != server.store() {
		t.Fatalf("indexador no reenganchado: vault=%q store=%p actual=%p", server.Indexer.Settings.VaultPath, server.Indexer.Store, server.store())
	}

	started := performAPI(t, handler, http.MethodPost, "/api/index/start", `{}`)
	if started.Code != http.StatusOK {
		t.Fatalf("inicio: status=%d cuerpo=%s", started.Code, started.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		progress := performAPI(t, handler, http.MethodGet, "/api/index/progress", "")
		payload := apiObject(t, progress)
		if payload["running"] == false {
			if payload["phase"] != "done" {
				t.Fatalf("progreso=%+v", payload)
			}
			result := payload["result"].(map[string]any)
			if result["indexed"].(float64) != 1 || result["scanned"].(float64) != 1 {
				t.Fatalf("resultado=%+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("la indexación no terminó: %+v", payload)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = initialStore.Close()
	if current := server.store(); current != nil {
		_ = current.Close()
	}
}

func TestExistingFrontendIsServedWithoutContractRewrite(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	server.Settings.BaseDir = t.TempDir()
	health := performAPI(t, server.Handler(), http.MethodGet, "/health", "")
	if health.Code != http.StatusOK || apiObject(t, health)["status"] != "ok" {
		t.Fatalf("health: status=%d cuerpo=%s", health.Code, health.Body.String())
	}
	home := performAPI(t, server.Handler(), http.MethodGet, "/", "")
	if home.Code != http.StatusOK || home.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(home.Body.String(), "/static/app.js") {
		t.Fatalf("home: status=%d cuerpo=%s", home.Code, home.Body.String())
	}
	modules := []struct {
		name     string
		contains string
	}{
		{"app.js", `./chat.js`},
		{"api.js", `export async function request`},
		{"chat.js", `/api/query/stream`},
		{"context-inspector.js", `createContextInspector`},
		{"projects.js", `createProjectsController`},
		{"retrieval-trace.js", `createTraceRenderer`},
		{"sources.js", `createSourceCard`},
		{"sse.js", `readServerEvents`},
		{"state.js", `createStore`},
	}
	for _, module := range modules {
		response := performAPI(t, server.Handler(), http.MethodGet, "/static/"+module.name, "")
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(response.Body.String(), module.contains) {
			t.Fatalf("%s: status=%d content-type=%q", module.name, response.Code, response.Header().Get("Content-Type"))
		}
	}
	styles := performAPI(t, server.Handler(), http.MethodGet, "/static/styles.css", "")
	if styles.Code != http.StatusOK || !strings.HasPrefix(styles.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("styles.css: status=%d content-type=%q", styles.Code, styles.Header().Get("Content-Type"))
	}
}

func TestHealthReadinessServerLimitsAndPaginationValidation(t *testing.T) {
	server, _ := queryServerFixture(t, .3)
	handler := server.Handler()
	live := performAPI(t, handler, http.MethodGet, "/health/live", "")
	ready := performAPI(t, handler, http.MethodGet, "/health/ready", "")
	if live.Code != http.StatusOK || ready.Code != http.StatusOK || apiObject(t, ready)["status"] != "ready" {
		t.Fatalf("live=%d ready=%d body=%s", live.Code, ready.Code, ready.Body.String())
	}
	configured := config.Settings{HTTPReadHeaderTimeoutSeconds: 7, HTTPIdleTimeoutSeconds: 55}
	httpServer := NewHTTPServer("127.0.0.1:0", handler, configured)
	if httpServer.ReadHeaderTimeout != 7*time.Second || httpServer.IdleTimeout != 55*time.Second || httpServer.MaxHeaderBytes != 1<<20 {
		t.Fatalf("server=%+v", httpServer)
	}
	invalid := performAPI(t, handler, http.MethodGet, "/api/conversations?limit=999999", "")
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pagination status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	cancel := performAPI(t, handler, http.MethodPost, "/api/index/cancel", `{}`)
	if cancel.Code != http.StatusConflict {
		t.Fatalf("cancel status=%d body=%s", cancel.Code, cancel.Body.String())
	}
}
