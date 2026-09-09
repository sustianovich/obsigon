package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/rag"
)

func (s *Server) store() *db.Store {
	if s.Store != nil {
		return s.Store
	}
	if s.Retriever != nil {
		return s.Retriever.Store
	}
	if s.Indexer != nil {
		return s.Indexer.Store
	}
	return nil
}

func (s *Server) selectChatProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile string `json:"profile"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	profile, ok := chatProfiles()[strings.TrimSpace(body.Profile)]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "No se pudo completar la operación."})
		return
	}
	model, err := config.ValidateLocalModelName(profile["model"].(string), "OLLAMA_CHAT_MODEL")
	if err != nil {
		s.internalError(w, "cambiar el perfil de chat", err)
		return
	}
	if err := updateEnvValue(filepath.Join(s.Settings.BaseDir, ".env"), "OLLAMA_CHAT_MODEL", model); err != nil {
		s.internalError(w, "guardar el perfil de chat", err)
		return
	}
	s.Settings.ChatModel = model
	if s.Indexer != nil {
		s.Indexer.Settings.ChatModel = model
	}
	if s.Retriever != nil {
		s.Retriever.Settings.ChatModel = model
	}
	if s.Memory != nil {
		s.Memory.Settings.ChatModel = model
	}
	if setter, ok := s.Generator.(interface{ SetChatModel(string) }); ok {
		setter.SetChatModel(model)
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "chat_model": model, "reindex_required": false})
}

func (s *Server) vaultPickerAvailable() bool {
	return s.VaultPicker != nil || nativeVaultPickerAvailable()
}

// vaultPickerTimeout limita cuánto puede tardar el subproceso del selector
// nativo. Es un margen amplio para no interrumpir a alguien eligiendo
// carpeta con calma, pero acota un cuelgue real (proceso sin sesión
// interactiva, bloqueado por el antivirus, etc.) para que no retenga
// recursos del servidor indefinidamente.
const vaultPickerTimeout = 5 * time.Minute

func (s *Server) pickVault(w http.ResponseWriter, r *http.Request) {
	picker := s.VaultPicker
	if picker == nil {
		if !nativeVaultPickerAvailable() {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"detail": "El selector nativo de carpetas no está disponible en este sistema."})
			return
		}
		picker = pickNativeVaultDirectory
	}

	s.runtimeMu.RLock()
	current := s.Settings.VaultPath
	s.runtimeMu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), vaultPickerTimeout)
	defer cancel()

	path, selected, err := picker(ctx, current)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.logErrorContext(r.Context(), "abrir el selector de vault", err)
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"detail": "El selector de carpetas no respondió a tiempo y se ha cerrado. Inténtalo de nuevo."})
			return
		}
		s.internalError(w, "abrir el selector de vault", err)
		return
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if !selected || path == "." {
		writeJSON(w, http.StatusOK, map[string]any{"selected": false, "reason": "cancelled"})
		return
	}
	if path == filepath.Clean(current) {
		writeJSON(w, http.StatusOK, map[string]any{"selected": false, "reason": "unchanged", "vault_path": path})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"selected": true, "vault_path": path, "vault_display_path": path})
}

// selectVault cambia el vault activo. La ruta llega desde el selector nativo
// de Windows o como texto en plataformas sin selector; el servidor la valida,
// la guarda en `.env` y reengancha el índice, la recuperación y la memoria al
// SQLite que corresponde a ese vault. El índice hay que reconstruirlo después.
func (s *Server) selectVault(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VaultPath string `json:"vault_path"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Solicitud no válida."})
		return
	}

	current := s.Settings.VaultPath
	requested := strings.TrimSpace(body.VaultPath)
	if requested == "" {
		_, statErr := os.Stat(current)
		writeJSON(w, http.StatusOK, map[string]any{"selected": false, "reason": "empty", "vault_path": current, "vault_display_path": current, "vault_name": filepath.Base(filepath.Clean(current)), "vault_exists": statErr == nil})
		return
	}

	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.Settings.BaseDir, target)
	}
	target = filepath.Clean(target)
	info, statErr := os.Stat(target)
	if statErr != nil || !info.IsDir() {
		s.logErrorContext(r.Context(), "elegir vault", errors.New("ruta inexistente o no es carpeta: "+target))
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "La ruta indicada no existe o no es una carpeta."})
		return
	}
	if target == current {
		writeJSON(w, http.StatusOK, map[string]any{"selected": false, "reason": "unchanged", "vault_path": current, "vault_display_path": current, "vault_name": filepath.Base(current), "vault_exists": true})
		return
	}

	newDatabase := s.Settings.DatabasePath
	if !s.Settings.DatabasePathExplicit {
		newDatabase = config.DefaultDatabasePath(s.Settings.BaseDir, target)
	}
	newStore, err := db.Open(newDatabase)
	if err != nil {
		s.internalError(w, "abrir la base del vault", err)
		return
	}
	if err := updateEnvValue(filepath.Join(s.Settings.BaseDir, ".env"), "OBSIDIAN_VAULT_PATH", filepath.ToSlash(target)); err != nil {
		_ = newStore.Close()
		s.internalError(w, "guardar la ruta del vault", err)
		return
	}

	previousStore := s.store()
	newSettings := s.Settings
	newSettings.VaultPath = target
	newSettings.DatabasePath = newDatabase

	var newIndexer *rag.Indexer
	if s.Indexer != nil {
		newIndexer = &rag.Indexer{Settings: newSettings, Store: newStore, Embedder: s.Indexer.Embedder}
	}
	var newRetriever *rag.Retriever
	if s.Retriever != nil {
		newRetriever = &rag.Retriever{
			Settings: newSettings,
			Store:    newStore,
			Vectors:  rag.NewVectorStore(newStore, newSettings.EmbeddingWorkers, rag.ANNConfig{Enabled: newSettings.ANNEnabled, Path: newSettings.DatabasePath + ".hnsw", M: newSettings.ANNM, EFConstruction: newSettings.ANNEFConstruction, EFSearch: newSettings.ANNEFSearch}),
			Embedder: s.Retriever.Embedder,
			Reranker: s.Retriever.Reranker,
		}
	}
	var newMemory *rag.MemoryManager
	if s.Memory != nil {
		newMemory = &rag.MemoryManager{Settings: newSettings, Store: newStore, Generator: s.Memory.Generator}
	}
	if s.Watcher != nil {
		s.Watcher.Retarget(target, newIndexer)
	}

	s.Settings = newSettings
	s.Store = newStore
	s.Indexer = newIndexer
	s.Retriever = newRetriever
	s.Memory = newMemory
	s.clearRetrievalTrace()

	// runtimeGuard ha esperado a que terminen todas las peticiones que usaban
	// el runtime anterior, y Retarget ha esperado al watcher. El cierre puede
	// hacerse ya sin depender de un temporizador arbitrario.
	if previousStore != nil && previousStore != newStore {
		_ = previousStore.Close()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"selected": true, "vault_path": target, "vault_display_path": target,
		"vault_name": filepath.Base(target), "vault_exists": true,
		"database_path": newDatabase, "reindex_required": true,
	})
}

func (s *Server) startIndex(w http.ResponseWriter, r *http.Request) {
	s.runtimeMu.RLock()
	locked := true
	defer func() {
		if locked {
			s.runtimeMu.RUnlock()
		}
	}()
	if s.Indexer == nil {
		s.internalError(w, "iniciar la indexación", errors.New("indexador no configurado"))
		return
	}
	if err := validateIndexVault(s.Indexer); err != nil {
		s.logErrorContext(r.Context(), "validar el vault antes de indexar", err)
		writeJSON(w, http.StatusConflict, map[string]any{"detail": "La carpeta del vault configurada no existe. Selecciona un vault válido."})
		return
	}
	if s.Progress == nil {
		s.Progress = NewIndexProgress()
	}
	if s.Progress.Start() {
		indexer := s.Indexer
		watcher := s.Watcher
		indexContext, cancel := context.WithCancel(context.Background())
		s.Progress.BindCancel(cancel)
		locked = false
		go func() {
			defer s.runtimeMu.RUnlock()
			defer cancel()
			var result rag.IndexResult
			var err error
			if watcher != nil {
				result, err = watcher.RunFullIndex(indexContext, s.Progress.Update)
			} else {
				result, err = indexer.IndexWithProgress(indexContext, s.Progress.Update)
			}
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.logError("indexar el vault en segundo plano", err)
				}
			}
			s.Progress.Finish(result, err)
		}()
	}
	writeJSON(w, http.StatusOK, s.Progress.Snapshot())
}

func validateIndexVault(indexer *rag.Indexer) error {
	if indexer == nil {
		return errors.New("indexador no configurado")
	}
	info, err := os.Stat(indexer.Settings.VaultPath)
	if err != nil {
		return fmt.Errorf("vault no disponible %q: %w", indexer.Settings.VaultPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("el vault configurado no es una carpeta: %q", indexer.Settings.VaultPath)
	}
	return nil
}

func (s *Server) cancelIndex(w http.ResponseWriter, _ *http.Request) {
	if s.Progress == nil {
		s.Progress = NewIndexProgress()
	}
	cancelled := s.Progress.Cancel()
	status := http.StatusOK
	if !cancelled {
		status = http.StatusConflict
	}
	writeJSON(w, status, s.Progress.Snapshot())
}

func (s *Server) indexProgress(w http.ResponseWriter, _ *http.Request) {
	if s.Progress == nil {
		s.Progress = NewIndexProgress()
	}
	writeJSON(w, http.StatusOK, s.Progress.Snapshot())
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	store := s.store()
	if store == nil {
		s.internalError(w, "listar proyectos", errors.New("SQLite no configurado"))
		return
	}
	items, err := store.ListProjects(r.Context())
	if err != nil {
		s.internalError(w, "listar proyectos", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": items, "default_project_id": db.DefaultProjectID})
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if decodeJSON(r, &body) != nil {
		writeJSON(w, 422, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	item, err := s.store().CreateProject(r.Context(), body.Name, s.Settings.VerifierContextTokens, s.Settings.WriterContextTokens)
	if err != nil {
		status := 400
		if errors.Is(err, db.ErrDuplicate) {
			status = 409
		}
		s.logErrorContext(r.Context(), "crear proyecto", err)
		writeJSON(w, status, map[string]any{"detail": "No se pudo completar la operación."})
		return
	}
	writeJSON(w, http.StatusCreated, item)
}
func (s *Server) renameProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if decodeJSON(r, &body) != nil {
		writeJSON(w, 422, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	item, err := s.store().RenameProject(r.Context(), r.PathValue("project_id"), body.Name)
	if err != nil {
		s.resourceError(w, r, err, "proyecto")
		return
	}
	writeJSON(w, 200, item)
}
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	item, err := s.store().DeleteProject(r.Context(), r.PathValue("project_id"))
	if err != nil {
		s.resourceError(w, r, err, "proyecto")
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) updateAgentSettings(w http.ResponseWriter, r *http.Request) {
	var body db.AgentSettings
	if decodeJSON(r, &body) != nil {
		writeJSON(w, 422, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	settings, err := s.store().UpdateAgentSettings(r.Context(), r.PathValue("project_id"), body)
	if err != nil {
		s.resourceError(w, r, err, "proyecto")
		return
	}
	memory, _ := s.store().ProjectMemoryStatus(r.Context(), r.PathValue("project_id"))
	writeJSON(w, 200, map[string]any{"project_id": r.PathValue("project_id"), "agent_settings": settings, "memory": memory})
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	var projectID *string
	if value := strings.TrimSpace(r.URL.Query().Get("project_id")); value != "" {
		projectID = &value
	}
	limit, offset, err := paginationQuery(r, s.Settings.HistoryPageSize, s.Settings.HistoryMaxPageSize)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Paginación no válida."})
		return
	}
	items, total, err := s.store().ListConversationsPage(r.Context(), projectID, limit, offset)
	if err != nil {
		s.internalError(w, "listar conversaciones", err)
		return
	}
	writeJSON(w, 200, map[string]any{"conversations": items, "pagination": map[string]any{"limit": limit, "offset": offset, "total": total, "has_more": offset+len(items) < total, "next_offset": offset + len(items)}})
}
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	limit, _, err := paginationQuery(r, min(100, max(10, s.Settings.HistoryPageSize*2)), s.Settings.HistoryMaxPageSize)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Paginación no válida."})
		return
	}
	beforeID := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("before_id")); raw != "" {
		beforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || beforeID < 1 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "Paginación no válida."})
			return
		}
	}
	item, err := s.store().GetConversationPage(r.Context(), r.PathValue("conversation_id"), beforeID, limit)
	if err != nil {
		s.resourceError(w, r, err, "conversación")
		return
	}
	writeJSON(w, 200, item)
}

func paginationQuery(r *http.Request, defaultLimit, maxLimit int) (int, int, error) {
	if defaultLimit <= 0 {
		defaultLimit = 50
	}
	if maxLimit < defaultLimit {
		maxLimit = max(defaultLimit, 200)
	}
	limit, offset := defaultLimit, 0
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxLimit {
			return 0, 0, errors.New("limit no válido")
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, errors.New("offset no válido")
		}
	}
	return limit, offset, nil
}
func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("conversation_id")
	deleted, err := s.store().DeleteConversation(r.Context(), id)
	if err != nil {
		s.internalError(w, "eliminar conversación", err)
		return
	}
	if !deleted {
		writeJSON(w, 404, map[string]any{"detail": "No existe la conversación solicitada."})
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "conversation_id": id})
}
func (s *Server) updateConversationMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if decodeJSON(r, &body) != nil {
		writeJSON(w, 422, map[string]any{"detail": "Solicitud no válida."})
		return
	}
	memory, err := s.store().SetConversationMemoryEnabled(r.Context(), r.PathValue("conversation_id"), body.Enabled)
	if err != nil {
		s.resourceError(w, r, err, "conversación")
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": r.PathValue("conversation_id"), "memory": memory})
}
func (s *Server) getConversationContext(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		s.internalError(w, "inspeccionar contexto", errors.New("memoria no configurada"))
		return
	}
	overview, err := s.Memory.ContextOverview(r.Context(), r.PathValue("conversation_id"))
	if err != nil {
		s.resourceError(w, r, err, "conversación")
		return
	}
	writeJSON(w, 200, overview)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("contenido JSON adicional")
		}
		return err
	}
	return nil
}
func (s *Server) resourceError(w http.ResponseWriter, r *http.Request, err error, resource string) {
	s.logErrorContext(r.Context(), "operar con "+resource, err)
	if errors.Is(err, db.ErrNotFound) {
		writeJSON(w, 404, map[string]any{"detail": "No existe el " + resource + " solicitado."})
		return
	}
	status := 400
	if errors.Is(err, db.ErrDuplicate) {
		status = 409
	}
	writeJSON(w, status, map[string]any{"detail": "No se pudo completar la operación."})
}

func chatProfiles() map[string]map[string]any {
	return map[string]map[string]any{
		"light":     {"id": "light", "label": "Ligero", "model": "qwen3.5:0.8b", "description": "Más rápido y con menor consumo.", "ram": "8 GB recomendados", "context_window": 32768, "reasoning": false, "think": false, "rerank_enabled": false, "rerank_backend": "noop"},
		"balanced":  {"id": "balanced", "label": "Equilibrado", "model": "qwen3.5:2b", "description": "Mejor comprensión con consumo moderado.", "ram": "8–12 GB recomendados", "context_window": 32768, "reasoning": false, "think": false, "rerank_enabled": false, "rerank_backend": "noop"},
		"quality":   {"id": "quality", "label": "Calidad", "model": "qwen3.5:4b", "description": "Mejor síntesis y seguimiento de instrucciones.", "ram": "12–16 GB recomendados", "context_window": 32768, "reasoning": false, "think": false, "rerank_enabled": false, "rerank_backend": "noop"},
		"qwen3_14b": {"id": "qwen3_14b", "label": "Qwen3 14B", "model": "qwen3:14b", "description": "Modelo local de mayor capacidad con razonamiento filtrado.", "ram": "16–20 GB recomendados", "context_window": 40960, "reasoning": true, "think": true, "rerank_enabled": false, "rerank_backend": "noop"},
		"llama3":    {"id": "llama3", "label": "Llama 3", "model": "llama3:latest", "description": "Buena redacción con contexto de 8K.", "ram": "16 GB recomendados", "context_window": 8192, "reasoning": false, "think": false, "rerank_enabled": false, "rerank_backend": "noop"},
		"gpt_oss":   {"id": "gpt_oss", "label": "GPT-OSS", "model": "gpt-oss:latest", "description": "Contexto amplio y razonamiento interno filtrado.", "ram": "24 GB recomendados", "context_window": 131072, "reasoning": true, "think": "low", "rerank_enabled": false, "rerank_backend": "noop"},
	}
}
func activeProfile(model string) any {
	for id, profile := range chatProfiles() {
		if strings.EqualFold(model, profile["model"].(string)) {
			return id
		}
	}
	return nil
}
func profileList() []map[string]any {
	profiles := chatProfiles()
	order := []string{"light", "balanced", "quality", "qwen3_14b", "llama3", "gpt_oss"}
	result := make([]map[string]any, 0, len(order))
	for _, id := range order {
		result = append(result, profiles[id])
	}
	return result
}

func updateEnvValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			lines[index] = key + "=" + value
			found = true
		}
	}
	if !found {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, key+"="+value)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600)
}
