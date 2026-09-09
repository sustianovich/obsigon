package rag

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

const (
	IndexFormatVersion  = 5
	ChunkerVersion      = "structured-3-outline"
	IndexFingerprintKey = "index_fingerprint"
	EmbeddingProvider   = "ollama"
)

var ErrIndexing = errors.New("ya hay una indexación en curso")

var excludedVaultDirectories = map[string]bool{".obsidian": true, "_plantillas": true, "_templates": true}

type PrefixScheme struct{ ID, Document, Query string }

var prefixSchemes = map[string]PrefixScheme{
	"none":  {ID: "none"},
	"nomic": {ID: "nomic", Document: "search_document: ", Query: "search_query: "},
	"e5":    {ID: "e5", Document: "passage: ", Query: "query: "},
	"bge":   {ID: "bge", Query: "Represent this sentence for searching relevant passages: "},
}

func ResolveScheme(modelName, configured string) (PrefixScheme, error) {
	requested := strings.ToLower(strings.TrimSpace(configured))
	if requested == "" || requested == "auto" {
		normalized := strings.ToLower(strings.TrimSpace(modelName))
		requested = "none"
		for _, rule := range [][2]string{{"nomic-embed", "nomic"}, {"multilingual-e5", "e5"}, {"intfloat/e5", "e5"}, {"e5-", "e5"}, {"bge-", "bge"}, {"bge:", "bge"}} {
			if strings.Contains(normalized, rule[0]) {
				requested = rule[1]
				break
			}
		}
	}
	scheme, ok := prefixSchemes[requested]
	if !ok {
		return PrefixScheme{}, fmt.Errorf("RAG_EMBEDDING_PREFIX_SCHEME no válido: %s", configured)
	}
	return scheme, nil
}

type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
	Dimension(context.Context) (int, error)
}

type Indexer struct {
	Settings config.Settings
	Store    *db.Store
	Embedder Embedder
	mu       sync.Mutex
}
type IndexResult struct {
	Scanned        int      `json:"scanned"`
	Indexed        int      `json:"indexed"`
	Unchanged      int      `json:"unchanged"`
	Deleted        int      `json:"deleted"`
	ChunksCreated  int      `json:"chunks_created"`
	ParentsCreated int      `json:"parents_created"`
	Errors         []string `json:"errors"`
	Rebuilt        bool     `json:"rebuilt"`
	RebuildReasons []string `json:"rebuild_reasons"`
	Stats          db.Stats `json:"stats"`
}

type IndexProgressEvent struct {
	Phase       string `json:"phase"`
	Processed   int    `json:"processed"`
	Total       int    `json:"total"`
	CurrentFile string `json:"current_file,omitempty"`
}

func BuildIndexConfiguration(settings config.Settings, dimension int, scheme PrefixScheme) map[string]any {
	result := map[string]any{"format_version": IndexFormatVersion, "vault_path": filepath.Clean(settings.VaultPath), "embedding_provider": EmbeddingProvider, "embedding_model": settings.EmbeddingModel, "embedding_dimension": dimension, "chunk_size": settings.ChunkSize, "chunk_overlap": settings.ChunkOverlap, "embedding_prefix_scheme": scheme.ID, "chunker_version": ChunkerVersion}
	if settings.ParentChildEnabled {
		result["chunking_mode"] = "parent_child-1"
		result["parent_chunk_size"] = settings.ParentChunkSize
		result["child_chunk_size"] = settings.ChildChunkSize
		result["child_chunk_overlap"] = settings.ChildChunkOverlap
	}
	return result
}

func SerializeIndexConfiguration(configuration map[string]any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "")
	if err := encoder.Encode(configuration); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func ChangedConfigurationFields(storedValue string, exists bool, current map[string]any) []string {
	if !exists || storedValue == "" {
		return []string{"configuración anterior no registrada"}
	}
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(storedValue), &stored); err != nil {
		return []string{"configuración anterior no válida"}
	}
	order := []string{"format_version", "vault_path", "embedding_provider", "embedding_model", "embedding_dimension", "chunk_size", "chunk_overlap", "embedding_prefix_scheme", "chunker_version", "chunking_mode", "parent_chunk_size", "child_chunk_size", "child_chunk_overlap"}
	changes := []string{}
	for _, key := range order {
		value, present := current[key]
		if !present {
			continue
		}
		if !jsonValueEqual(stored[key], value) {
			changes = append(changes, key)
		}
	}
	if _, was := stored["chunking_mode"]; was {
		if _, now := current["chunking_mode"]; !now {
			changes = append(changes, "chunking_mode")
		}
	}
	return changes
}

func jsonValueEqual(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

// isExcludedPathPart indica si un segmento de ruta (un componente entre "/")
// debe excluirse del vault: carpetas ocultas, `.obsidian`, `_plantillas` y
// `_templates`. La usan tanto el descubrimiento por barrido como el watcher
// por eventos para aplicar exactamente las mismas exclusiones.
func isExcludedPathPart(part string) bool {
	return strings.HasPrefix(part, ".") || excludedVaultDirectories[strings.ToLower(part)]
}

// hasExcludedSegment indica si alguna carpeta de una ruta relativa (con "/")
// está excluida del vault.
func hasExcludedSegment(relativeSlash string) bool {
	for _, part := range strings.FieldsFunc(relativeSlash, func(r rune) bool { return r == '/' }) {
		if isExcludedPathPart(part) {
			return true
		}
	}
	return false
}

func DiscoverMarkdownFiles(vaultPath string) ([]string, error) {
	info, err := os.Stat(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("no existe la carpeta de Obsidian: %s", vaultPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("la ruta de Obsidian no es una carpeta: %s", vaultPath)
	}
	files := []string{}
	err = filepath.WalkDir(vaultPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == vaultPath {
			return nil
		}
		relative, err := filepath.Rel(vaultPath, path)
		if err != nil {
			return err
		}
		relativeSlash := filepath.ToSlash(relative)
		if hasExcludedSegment(relativeSlash) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i]) < strings.ToLower(files[j]) })
	return files, nil
}

func (i *Indexer) Index(ctx context.Context) (IndexResult, error) {
	return i.IndexWithProgress(ctx, nil)
}

func (i *Indexer) IndexWithProgress(ctx context.Context, onProgress func(IndexProgressEvent)) (IndexResult, error) {
	if !i.mu.TryLock() {
		return IndexResult{}, ErrIndexing
	}
	defer i.mu.Unlock()
	reportIndexProgress(onProgress, IndexProgressEvent{Phase: "scanning"})
	files, err := DiscoverMarkdownFiles(i.Settings.VaultPath)
	if err != nil {
		return IndexResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return IndexResult{}, err
	}
	reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Total: len(files)})
	dimension, err := i.Embedder.Dimension(ctx)
	if err != nil {
		return IndexResult{}, err
	}
	scheme, err := ResolveScheme(i.Settings.EmbeddingModel, i.Settings.EmbeddingPrefixScheme)
	if err != nil {
		return IndexResult{}, err
	}
	configuration := BuildIndexConfiguration(i.Settings, dimension, scheme)
	serialized, err := SerializeIndexConfiguration(configuration)
	if err != nil {
		return IndexResult{}, err
	}
	stored, exists, err := i.Store.Meta(ctx, IndexFingerprintKey)
	if err != nil {
		return IndexResult{}, err
	}
	changes := ChangedConfigurationFields(stored, exists, configuration)
	hashes, err := i.Store.DocumentHashes(ctx)
	if err != nil {
		return IndexResult{}, err
	}
	if len(changes) > 0 {
		return i.atomicRebuild(ctx, files, serialized, changes, exists, hashes, scheme, onProgress)
	}
	return i.incremental(ctx, files, hashes, scheme, onProgress)
}

func reportIndexProgress(callback func(IndexProgressEvent), event IndexProgressEvent) {
	if callback != nil {
		callback(event)
	}
}

func (i *Indexer) prepare(ctx context.Context, path string, scheme PrefixScheme) (db.Document, error) {
	document, err := ParseMarkdownFile(path, ParseOptions{ChunkSize: i.Settings.ChunkSize, ChunkOverlap: i.Settings.ChunkOverlap, ParentChildEnabled: i.Settings.ParentChildEnabled, ParentChunkSize: i.Settings.ParentChunkSize, ChildChunkSize: i.Settings.ChildChunkSize, ChildChunkOverlap: i.Settings.ChildChunkOverlap})
	if err != nil {
		return db.Document{}, err
	}
	supplement := ""
	if document.Supplement != "" {
		supplement = "\n\n" + document.Supplement
	}
	inputs := make([]string, len(document.Chunks))
	for index, chunk := range document.Chunks {
		inputs[index] = scheme.Document + "Título: " + document.Title + "\nSección: " + chunk.Heading + "\n\n" + chunk.Content + supplement
	}
	vectors, err := i.Embedder.Embed(ctx, inputs)
	if err != nil {
		return db.Document{}, err
	}
	if len(vectors) != len(document.Chunks) {
		return db.Document{}, errors.New("Ollama devolvió un número de embeddings distinto al esperado")
	}
	relative, err := filepath.Rel(i.Settings.VaultPath, path)
	if err != nil {
		return db.Document{}, err
	}
	result := db.Document{Path: filepath.ToSlash(relative), Title: document.Title, SHA256: document.SHA256, MTime: document.MTime, Metadata: document.Metadata}
	for _, link := range document.Links {
		result.Links = append(result.Links, db.Link{Target: link.Target, Section: link.Section, Alias: link.Alias, Embedded: link.Embedded})
	}
	for _, parent := range document.Parents {
		result.Parents = append(result.Parents, db.Parent{Index: parent.Index, Heading: parent.Heading, Content: parent.Content})
	}
	for index, chunk := range document.Chunks {
		result.Chunks = append(result.Chunks, db.Chunk{Index: chunk.Index, Heading: chunk.Heading, Content: chunk.Content, ParentIndex: chunk.ParentIndex, Embedding: vectors[index]})
	}
	return result, nil
}

func (i *Indexer) atomicRebuild(ctx context.Context, files []string, fingerprint string, changes []string, hadStored bool, known map[string]string, scheme PrefixScheme, onProgress func(IndexProgressEvent)) (IndexResult, error) {
	runID, err := newRunID()
	if err != nil {
		return IndexResult{}, err
	}
	if err := i.Store.ResetStaged(ctx); err != nil {
		return IndexResult{}, err
	}
	result := IndexResult{Scanned: len(files), RebuildReasons: changes, Errors: []string{}}
	for index, path := range files {
		if err := ctx.Err(); err != nil {
			_ = i.Store.DiscardStaged(context.Background(), runID)
			return result, err
		}
		relative, _ := filepath.Rel(i.Settings.VaultPath, path)
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index, Total: len(files), CurrentFile: filepath.ToSlash(relative)})
		document, err := i.prepare(ctx, path, scheme)
		if err != nil {
			if ctx.Err() != nil {
				_ = i.Store.DiscardStaged(context.Background(), runID)
				return result, ctx.Err()
			}
			result.Errors = append(result.Errors, filepath.Base(path)+": No se pudo indexar este documento.")
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: filepath.ToSlash(relative)})
			continue
		}
		if err := i.Store.StageDocument(ctx, runID, document); err != nil {
			result.Errors = append(result.Errors, document.Path+": No se pudo indexar este documento.")
			continue
		}
		result.Indexed++
		result.ChunksCreated += len(document.Chunks)
		result.ParentsCreated += len(document.Parents)
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: document.Path})
	}
	if len(result.Errors) > 0 {
		_ = i.Store.DiscardStaged(ctx, runID)
		result.Indexed, result.ChunksCreated, result.ParentsCreated = 0, 0, 0
		result.Errors = append(result.Errors, "Reconstrucción atómica cancelada: el índice anterior se ha conservado sin cambios.")
		result.Stats, _ = i.Store.Stats(ctx)
		return result, nil
	}
	reportIndexProgress(onProgress, IndexProgressEvent{Phase: "committing", Processed: len(files), Total: len(files)})
	links, err := i.Store.ResolveStagedLinks(ctx, runID)
	if err != nil {
		_ = i.Store.DiscardStaged(ctx, runID)
		return IndexResult{}, err
	}
	expected := db.Stats{Documents: result.Indexed, Chunks: result.ChunksCreated, Parents: result.ParentsCreated, Links: links}
	if err := i.Store.CommitStagedIndex(ctx, runID, IndexFingerprintKey, fingerprint, expected); err != nil {
		_ = i.Store.DiscardStaged(ctx, runID)
		result.Indexed, result.ChunksCreated, result.ParentsCreated = 0, 0, 0
		result.Errors = append(result.Errors, "No se pudo activar el índice preparado; el índice anterior se ha conservado sin cambios.")
		result.Stats, _ = i.Store.Stats(ctx)
		return result, nil
	}
	current := map[string]bool{}
	for _, path := range files {
		relative, _ := filepath.Rel(i.Settings.VaultPath, path)
		current[filepath.ToSlash(relative)] = true
	}
	for path := range known {
		if !current[path] {
			result.Deleted++
		}
	}
	result.Rebuilt = hadStored || len(known) > 0
	if !result.Rebuilt {
		result.RebuildReasons = nil
	}
	result.Stats, err = i.Store.Stats(ctx)
	return result, err
}

func (i *Indexer) incremental(ctx context.Context, files []string, known map[string]string, scheme PrefixScheme, onProgress func(IndexProgressEvent)) (IndexResult, error) {
	paths := map[string]bool{}
	for _, path := range files {
		relative, _ := filepath.Rel(i.Settings.VaultPath, path)
		paths[filepath.ToSlash(relative)] = true
	}
	deleted, err := i.Store.DeleteDocumentsNotIn(ctx, paths)
	if err != nil {
		return IndexResult{}, err
	}
	result := IndexResult{Scanned: len(files), Deleted: deleted, Errors: []string{}}
	for index, path := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		relative, _ := filepath.Rel(i.Settings.VaultPath, path)
		relative = filepath.ToSlash(relative)
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index, Total: len(files), CurrentFile: relative})
		hash, err := CalculateSHA256(path)
		if err != nil {
			result.Errors = append(result.Errors, relative+": No se pudo indexar este documento.")
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: relative})
			continue
		}
		if known[relative] == hash {
			result.Unchanged++
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: relative})
			continue
		}
		document, err := i.prepare(ctx, path, scheme)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Errors = append(result.Errors, relative+": No se pudo indexar este documento.")
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: relative})
			continue
		}
		if err := i.Store.ReplaceDocument(ctx, document); err != nil {
			result.Errors = append(result.Errors, relative+": No se pudo indexar este documento.")
			continue
		}
		result.Indexed++
		result.ChunksCreated += len(document.Chunks)
		result.ParentsCreated += len(document.Parents)
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: index + 1, Total: len(files), CurrentFile: relative})
	}
	reportIndexProgress(onProgress, IndexProgressEvent{Phase: "committing", Processed: len(files), Total: len(files)})
	result.Stats, err = i.Store.Stats(ctx)
	return result, err
}

// IndexPaths reindexa sólo el lote dado en vez de enumerar el vault completo.
// changedPaths y deletedPaths son rutas relativas al vault con separador "/",
// como las que expone DiscoverMarkdownFiles tras normalizarlas. Si la
// configuración de indexación cambió desde la última ejecución (modelo,
// chunker, prefijos...), delega en Index para forzar una reconstrucción
// atómica completa: un lote dirigido no puede reconciliar eso por sí solo.
func (i *Indexer) IndexPaths(ctx context.Context, changedPaths, deletedPaths []string, onProgress func(IndexProgressEvent)) (IndexResult, error) {
	if !i.mu.TryLock() {
		return IndexResult{}, ErrIndexing
	}
	unlocked := false
	defer func() {
		if !unlocked {
			i.mu.Unlock()
		}
	}()
	if len(changedPaths) == 0 && len(deletedPaths) == 0 {
		return IndexResult{Errors: []string{}}, nil
	}
	dimension, err := i.Embedder.Dimension(ctx)
	if err != nil {
		return IndexResult{}, err
	}
	scheme, err := ResolveScheme(i.Settings.EmbeddingModel, i.Settings.EmbeddingPrefixScheme)
	if err != nil {
		return IndexResult{}, err
	}
	configuration := BuildIndexConfiguration(i.Settings, dimension, scheme)
	stored, exists, err := i.Store.Meta(ctx, IndexFingerprintKey)
	if err != nil {
		return IndexResult{}, err
	}
	if changes := ChangedConfigurationFields(stored, exists, configuration); len(changes) > 0 {
		i.mu.Unlock()
		unlocked = true
		return i.Index(ctx)
	}
	changed := normalizeRelativePaths(changedPaths)
	deleted := normalizeRelativePaths(deletedPaths)
	for path := range changed {
		delete(deleted, path)
	}
	result := IndexResult{Scanned: len(changed), Errors: []string{}}
	total := len(changed)
	processed := 0
	reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Total: total})
	for _, relative := range sortedKeys(changed) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		absolute := filepath.Join(i.Settings.VaultPath, filepath.FromSlash(relative))
		info, statErr := os.Stat(absolute)
		if statErr != nil || info.IsDir() {
			// El archivo desapareció o fue sustituido por un directorio entre el
			// evento y este lote: se trata como borrado en vez de fallar el lote.
			deleted[relative] = true
			processed++
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: processed, Total: total, CurrentFile: relative})
			continue
		}
		document, err := i.prepare(ctx, absolute, scheme)
		if err != nil {
			result.Errors = append(result.Errors, relative+": No se pudo indexar este documento.")
			processed++
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: processed, Total: total, CurrentFile: relative})
			continue
		}
		if err := i.Store.ReplaceDocument(ctx, document); err != nil {
			result.Errors = append(result.Errors, relative+": No se pudo indexar este documento.")
			processed++
			reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: processed, Total: total, CurrentFile: relative})
			continue
		}
		result.Indexed++
		result.ChunksCreated += len(document.Chunks)
		result.ParentsCreated += len(document.Parents)
		processed++
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "embedding", Processed: processed, Total: total, CurrentFile: relative})
	}
	if len(deleted) > 0 {
		reportIndexProgress(onProgress, IndexProgressEvent{Phase: "committing", Processed: total, Total: total})
		removed, err := i.Store.DeleteDocuments(ctx, sortedKeys(deleted))
		if err != nil {
			return result, err
		}
		result.Deleted = removed
	}
	result.Stats, err = i.Store.Stats(ctx)
	return result, err
}

func normalizeRelativePaths(paths []string) map[string]bool {
	result := make(map[string]bool, len(paths))
	for _, path := range paths {
		normalized := filepath.ToSlash(strings.TrimSpace(path))
		if normalized != "" {
			result[normalized] = true
		}
	}
	return result
}

func newRunID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
