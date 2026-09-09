package rag

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

type indexRunner interface {
	Index(context.Context) (IndexResult, error)
	IndexPaths(ctx context.Context, changedPaths, deletedPaths []string, onProgress func(IndexProgressEvent)) (IndexResult, error)
}

// VaultWatcher vigila el vault con eventos nativos del sistema de archivos
// (fsnotify, sin CGO). Acumula durante un debounce las rutas tocadas por una
// ráfaga de altas, cambios, renombrados y borrados, y reindexa sólo ese lote
// con Indexer.IndexPaths: nunca enumera ni calcula hashes del resto del
// vault. Una reconciliación completa periódica repara eventos perdidos
// (desbordamientos del watcher, unidades de red sin soporte de
// notificaciones, cambios ocurridos durante una suspensión del equipo). Si
// fsnotify no puede vigilar el vault, cae a un sondeo de respaldo que sigue
// pasando por el mismo debounce y el mismo IndexPaths.
type VaultWatcher struct {
	debounce          time.Duration
	reconcileInterval time.Duration
	operationMu       sync.Mutex
	generation        atomic.Int64

	mu             sync.RWMutex
	vault          string
	indexer        indexRunner
	runCtx         context.Context
	state          string
	backend        string
	lastError      string
	lastIndexed    *time.Time
	lastReconciled *time.Time
	lastBatchSize  int
	trackedFiles   map[string]bool

	watchMu   sync.Mutex
	fsWatcher *fsnotify.Watcher
	watchDirs map[string]bool

	pendingMu           sync.Mutex
	pending             map[string]bool
	timer               *time.Timer
	batchDeferred       bool
	batchRetryAttempts  int
	nextBatchRetry      *time.Time
	reconcileTimer      *time.Timer
	reconcileRetryCount int
	nextReconcileRetry  *time.Time
}

const (
	fallbackPollInterval = 2 * time.Second
	minIndexRetryBackoff = 100 * time.Millisecond
	maxIndexRetryBackoff = 30 * time.Second
)

// NewVaultWatcher crea un watcher. `durations` es opcional: el primer valor
// es el debounce (por defecto 750ms) y el segundo el intervalo de
// reconciliación completa (por defecto 5 minutos).
func NewVaultWatcher(vault string, indexer indexRunner, durations ...time.Duration) *VaultWatcher {
	debounce, reconcile := 750*time.Millisecond, 5*time.Minute
	if len(durations) > 0 && durations[0] > 0 {
		debounce = durations[0]
	}
	if len(durations) > 1 && durations[1] > 0 {
		reconcile = durations[1]
	}
	return &VaultWatcher{
		vault: vault, indexer: indexer, debounce: debounce, reconcileInterval: reconcile,
		state: "up_to_date", backend: "events", trackedFiles: map[string]bool{},
	}
}

// Retarget cambia el vault vigilado en caliente: espera a que termine
// cualquier indexación en curso, sustituye atómicamente el indexador y los
// watches de fsnotify por los del nuevo vault, y descarta cualquier lote
// pendiente del vault anterior.
func (w *VaultWatcher) Retarget(vault string, indexers ...indexRunner) {
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	w.mu.Lock()
	w.vault, w.state, w.lastError = vault, "indexing", ""
	w.trackedFiles = map[string]bool{}
	w.lastReconciled = nil
	if len(indexers) > 0 && indexers[0] != nil {
		w.indexer = indexers[0]
	}
	ctx := w.runCtx
	w.mu.Unlock()
	w.discardPending()
	if ctx != nil && ctx.Err() == nil {
		w.mount(ctx)
	}
}

func (w *VaultWatcher) target() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.vault
}

func (w *VaultWatcher) runner() indexRunner {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.indexer
}

func (w *VaultWatcher) currentRunCtx() context.Context {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.runCtx
}

func (w *VaultWatcher) Run(ctx context.Context) {
	w.mu.Lock()
	w.runCtx = ctx
	w.mu.Unlock()
	w.mount(ctx)
	ticker := time.NewTicker(w.reconcileInterval)
	defer ticker.Stop()
	defer w.teardown()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcile(ctx)
		}
	}
}

func (w *VaultWatcher) teardown() {
	w.generation.Add(1)
	w.watchMu.Lock()
	w.closeFSWatcherLocked()
	w.watchMu.Unlock()
	w.discardPending()
}

func (w *VaultWatcher) discardPending() {
	w.pendingMu.Lock()
	w.pending = nil
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.batchDeferred, w.batchRetryAttempts, w.nextBatchRetry = false, 0, nil
	if w.reconcileTimer != nil {
		w.reconcileTimer.Stop()
		w.reconcileTimer = nil
	}
	w.reconcileRetryCount, w.nextReconcileRetry = 0, nil
	w.pendingMu.Unlock()
}

func (w *VaultWatcher) closeFSWatcherLocked() {
	if w.fsWatcher != nil {
		_ = w.fsWatcher.Close()
		w.fsWatcher = nil
	}
	w.watchDirs = nil
}

// mount (re)monta la vigilancia del vault actual: cierra los watches
// anteriores, recalcula el conjunto de Markdown conocido con un único
// barrido y arranca fsnotify sobre cada directorio no excluido. Si fsnotify
// no puede vigilar el vault (permisos, unidad de red, límite de watches del
// sistema...) cae a un sondeo de respaldo que sigue agrupando por debounce.
func (w *VaultWatcher) mount(ctx context.Context) {
	generation := w.generation.Add(1)
	vault := w.target()

	w.watchMu.Lock()
	w.closeFSWatcherLocked()
	w.watchMu.Unlock()

	files, err := DiscoverMarkdownFiles(vault)
	known := map[string]bool{}
	if err == nil {
		for _, path := range files {
			if relative, relErr := filepath.Rel(vault, path); relErr == nil {
				known[filepath.ToSlash(relative)] = true
			}
		}
	}
	w.mu.Lock()
	w.trackedFiles = known
	w.mu.Unlock()

	fsWatcher, watchErr := fsnotify.NewWatcher()
	if watchErr != nil {
		w.mountPollingFallback(ctx, generation, "No se pudo iniciar la vigilancia por eventos del sistema de archivos.")
		return
	}
	dirs := map[string]bool{}
	_ = filepath.WalkDir(vault, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return nil
		}
		if path != vault {
			relative, relErr := filepath.Rel(vault, path)
			if relErr == nil && hasExcludedSegment(filepath.ToSlash(relative)) {
				return filepath.SkipDir
			}
		}
		if addErr := fsWatcher.Add(path); addErr == nil {
			dirs[path] = true
		}
		return nil
	})
	if len(dirs) == 0 {
		_ = fsWatcher.Close()
		w.mountPollingFallback(ctx, generation, "No se pudo vigilar ningún directorio del vault.")
		return
	}
	w.watchMu.Lock()
	w.fsWatcher = fsWatcher
	w.watchDirs = dirs
	w.watchMu.Unlock()
	w.mu.Lock()
	w.backend, w.state, w.lastError = "events", "up_to_date", ""
	w.mu.Unlock()
	go w.pump(ctx, fsWatcher, generation)
}

func (w *VaultWatcher) mountPollingFallback(ctx context.Context, generation int64, reason string) {
	w.mu.Lock()
	w.backend, w.state, w.lastError = "polling", "up_to_date", reason
	w.mu.Unlock()
	go w.pollLoop(ctx, generation)
}

func (w *VaultWatcher) pump(ctx context.Context, watcher *fsnotify.Watcher, generation int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok || w.generation.Load() != generation {
				return
			}
			w.handleEvent(watcher, event)
		case err, ok := <-watcher.Errors:
			if !ok || w.generation.Load() != generation {
				return
			}
			if err != nil {
				w.setError("Error del vigilante de archivos: " + err.Error())
			}
		}
	}
}

func (w *VaultWatcher) handleEvent(watcher *fsnotify.Watcher, event fsnotify.Event) {
	vault := w.target()
	relative, err := filepath.Rel(vault, event.Name)
	if err != nil {
		return
	}
	relativeSlash := filepath.ToSlash(relative)
	if relativeSlash == "." || hasExcludedSegment(relativeSlash) {
		return
	}
	if event.Has(fsnotify.Create) {
		if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
			w.watchSubtree(watcher, vault, event.Name)
			w.touchMarkdownTree(vault, event.Name)
			return
		}
	}
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		w.watchMu.Lock()
		_, wasDir := w.watchDirs[event.Name]
		if wasDir {
			delete(w.watchDirs, event.Name)
		}
		w.watchMu.Unlock()
		if wasDir {
			_ = watcher.Remove(event.Name)
			w.touchKnownUnderPrefix(relativeSlash)
			return
		}
	}
	if strings.EqualFold(filepath.Ext(relativeSlash), ".md") {
		w.touch(relativeSlash)
	}
}

// watchSubtree añade watches para un directorio nuevo (y sus subdirectorios)
// aparecido dentro del vault, por ejemplo tras mover o copiar una carpeta.
func (w *VaultWatcher) watchSubtree(watcher *fsnotify.Watcher, vault, root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return nil
		}
		if path != vault {
			relative, relErr := filepath.Rel(vault, path)
			if relErr == nil && hasExcludedSegment(filepath.ToSlash(relative)) {
				return filepath.SkipDir
			}
		}
		if err := watcher.Add(path); err == nil {
			w.watchMu.Lock()
			w.watchDirs[path] = true
			w.watchMu.Unlock()
		}
		return nil
	})
}

// touchMarkdownTree marca como tocados todos los Markdown bajo un directorio
// recién aparecido, para que el debounce los reindexe como altas.
func (w *VaultWatcher) touchMarkdownTree(vault, root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		relative, relErr := filepath.Rel(vault, path)
		if relErr != nil {
			return nil
		}
		relativeSlash := filepath.ToSlash(relative)
		if entry.IsDir() {
			if path != vault && hasExcludedSegment(relativeSlash) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			w.touch(relativeSlash)
		}
		return nil
	})
}

// touchKnownUnderPrefix marca como tocado (y por tanto, tras comprobar en
// disco, como borrado) cada Markdown conocido bajo un directorio que acaba
// de desaparecer o renombrarse.
func (w *VaultWatcher) touchKnownUnderPrefix(relativePrefix string) {
	prefix := strings.TrimSuffix(relativePrefix, "/") + "/"
	w.mu.RLock()
	matches := make([]string, 0, len(w.trackedFiles))
	for path := range w.trackedFiles {
		if strings.HasPrefix(path, prefix) {
			matches = append(matches, path)
		}
	}
	w.mu.RUnlock()
	for _, path := range matches {
		w.touch(path)
	}
}

// touch acumula una ruta relativa tocada y (re)arma el temporizador de
// debounce. El estado final (cambiado o borrado) se resuelve en disco cuando
// el debounce vence, no aquí: eso hace irrelevante el tipo exacto de evento
// y el orden en que llegan Create/Write/Rename/Remove.
func (w *VaultWatcher) touch(relativeSlash string) {
	w.pendingMu.Lock()
	if w.pending == nil {
		w.pending = map[string]bool{}
	}
	w.pending[relativeSlash] = true
	if w.timer == nil {
		w.timer = time.AfterFunc(w.debounce, w.flush)
	} else if !w.batchDeferred {
		w.timer.Reset(w.debounce)
	}
	w.pendingMu.Unlock()
	w.mu.Lock()
	if w.state != "indexing" {
		w.state, w.lastError = "pending", ""
	}
	w.mu.Unlock()
}

func (w *VaultWatcher) flush() {
	w.pendingMu.Lock()
	touched := w.pending
	w.pending, w.timer = nil, nil
	w.batchDeferred, w.nextBatchRetry = false, nil
	w.pendingMu.Unlock()
	if len(touched) == 0 {
		return
	}
	ctx := w.currentRunCtx()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	w.processBatch(ctx, touched)
}

func (w *VaultWatcher) processBatch(ctx context.Context, touched map[string]bool) {
	vault := w.target()
	changed := make([]string, 0, len(touched))
	deleted := make([]string, 0, len(touched))
	for relative := range touched {
		if hasExcludedSegment(relative) {
			continue
		}
		absolute := filepath.Join(vault, filepath.FromSlash(relative))
		if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
			changed = append(changed, relative)
		} else {
			deleted = append(deleted, relative)
		}
	}
	if len(changed) == 0 && len(deleted) == 0 {
		w.finishQueuedOperation(false)
		return
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	w.mu.Lock()
	w.state, w.lastBatchSize = "indexing", len(changed)+len(deleted)
	w.mu.Unlock()
	result, err := w.runner().IndexPaths(ctx, changed, deleted, nil)
	if errors.Is(err, ErrIndexing) {
		w.deferBatch(touched)
		return
	}
	if err != nil || len(result.Errors) > 0 {
		w.clearBatchRetryIfIdle()
		w.setError("No se pudo completar la indexación dirigida.")
		return
	}
	timestamp := time.Now().UTC()
	w.mu.Lock()
	for _, path := range changed {
		w.trackedFiles[path] = true
	}
	for _, path := range deleted {
		delete(w.trackedFiles, path)
	}
	w.lastIndexed = &timestamp
	w.mu.Unlock()
	w.finishQueuedOperation(true)
}

// RunFullIndex serializa una indexacion completa iniciada desde fuera del
// watcher con los lotes dirigidos y las reconciliaciones. El TryLock interno
// del Indexer se conserva como ultima defensa para otros llamantes.
func (w *VaultWatcher) RunFullIndex(ctx context.Context, onProgress func(IndexProgressEvent)) (IndexResult, error) {
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return IndexResult{}, err
	}
	runner := w.runner()
	if progressRunner, ok := runner.(interface {
		IndexWithProgress(context.Context, func(IndexProgressEvent)) (IndexResult, error)
	}); ok {
		return progressRunner.IndexWithProgress(ctx, onProgress)
	}
	return runner.Index(ctx)
}

func (w *VaultWatcher) deferBatch(touched map[string]bool) {
	w.pendingMu.Lock()
	if w.pending == nil {
		w.pending = map[string]bool{}
	}
	for path := range touched {
		w.pending[path] = true
	}
	w.batchRetryAttempts++
	delay := indexRetryBackoff(w.debounce, w.batchRetryAttempts)
	retryAt := time.Now().UTC().Add(delay)
	w.batchDeferred, w.nextBatchRetry = true, &retryAt
	if w.timer == nil {
		w.timer = time.AfterFunc(delay, w.flush)
	} else {
		w.timer.Reset(delay)
	}
	w.pendingMu.Unlock()
	w.mu.Lock()
	w.state, w.lastError = "deferred", ""
	w.mu.Unlock()
}

func (w *VaultWatcher) clearBatchRetryIfIdle() {
	w.pendingMu.Lock()
	if !w.batchDeferred {
		w.batchRetryAttempts = 0
	}
	w.pendingMu.Unlock()
}

func (w *VaultWatcher) finishQueuedOperation(resetBatchRetry bool) {
	w.pendingMu.Lock()
	if resetBatchRetry && !w.batchDeferred {
		w.batchRetryAttempts = 0
	}
	pending := len(w.pending)
	deferred := w.batchDeferred || w.reconcileTimer != nil
	w.pendingMu.Unlock()
	w.mu.Lock()
	switch {
	case deferred:
		w.state, w.lastError = "deferred", ""
	case pending > 0:
		w.state, w.lastError = "pending", ""
	default:
		w.state, w.lastError = "up_to_date", ""
	}
	w.mu.Unlock()
}

func indexRetryBackoff(debounce time.Duration, attempt int) time.Duration {
	delay := debounce
	if delay < minIndexRetryBackoff {
		delay = minIndexRetryBackoff
	}
	for current := 1; current < attempt && delay < maxIndexRetryBackoff; current++ {
		if delay > maxIndexRetryBackoff/2 {
			return maxIndexRetryBackoff
		}
		delay *= 2
	}
	if delay > maxIndexRetryBackoff {
		return maxIndexRetryBackoff
	}
	return delay
}

// reconcile ejecuta una indexación completa poco frecuente para reparar
// eventos perdidos, y remonta la vigilancia por si aparecieron o
// desaparecieron directorios que el watcher no vio en tiempo real.
func (w *VaultWatcher) reconcile(ctx context.Context) {
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	w.mu.Lock()
	previousBackend := w.backend
	w.backend = "reconciling"
	w.mu.Unlock()
	result, err := w.runner().Index(ctx)
	timestamp := time.Now().UTC()
	if errors.Is(err, ErrIndexing) {
		w.scheduleReconcileRetry()
		w.mu.Lock()
		w.backend, w.state, w.lastError, w.lastReconciled = previousBackend, "deferred", "", &timestamp
		w.mu.Unlock()
		return
	}
	w.clearReconcileRetry()
	if ctx.Err() == nil {
		w.mount(ctx)
	}
	w.mu.Lock()
	w.lastReconciled = &timestamp
	if err != nil || len(result.Errors) > 0 {
		w.state, w.lastError = "error", "No se pudo completar la reconciliación periódica."
	} else {
		w.lastIndexed = &timestamp
	}
	w.mu.Unlock()
	if err == nil && len(result.Errors) == 0 {
		w.finishQueuedOperation(true)
	}
}

func (w *VaultWatcher) scheduleReconcileRetry() {
	w.pendingMu.Lock()
	w.reconcileRetryCount++
	delay := indexRetryBackoff(w.debounce, w.reconcileRetryCount)
	retryAt := time.Now().UTC().Add(delay)
	w.nextReconcileRetry = &retryAt
	if w.reconcileTimer == nil {
		w.reconcileTimer = time.AfterFunc(delay, w.retryReconcile)
	} else {
		w.reconcileTimer.Reset(delay)
	}
	w.pendingMu.Unlock()
}

func (w *VaultWatcher) retryReconcile() {
	w.pendingMu.Lock()
	w.reconcileTimer, w.nextReconcileRetry = nil, nil
	w.pendingMu.Unlock()
	ctx := w.currentRunCtx()
	if ctx != nil && ctx.Err() == nil {
		w.reconcile(ctx)
	}
}

func (w *VaultWatcher) clearReconcileRetry() {
	w.pendingMu.Lock()
	if w.reconcileTimer != nil {
		w.reconcileTimer.Stop()
	}
	w.reconcileTimer, w.nextReconcileRetry, w.reconcileRetryCount = nil, nil, 0
	w.pendingMu.Unlock()
}

func (w *VaultWatcher) setError(message string) {
	w.mu.Lock()
	w.state, w.lastError = "error", message
	w.mu.Unlock()
}

func (w *VaultWatcher) Status() map[string]any {
	w.pendingMu.Lock()
	pendingPaths := len(w.pending)
	nextRetry := earlierTime(w.nextBatchRetry, w.nextReconcileRetry)
	w.pendingMu.Unlock()
	w.mu.RLock()
	defer w.mu.RUnlock()
	return map[string]any{
		"enabled": true, "state": w.state, "backend": w.backend, "error": nilIfBlank(w.lastError),
		"last_indexed_at": w.lastIndexed, "last_reconciled_at": w.lastReconciled,
		"debounce_ms": w.debounce.Milliseconds(), "reconcile_interval_ms": w.reconcileInterval.Milliseconds(),
		"last_batch_size": w.lastBatchSize, "tracked_files": len(w.trackedFiles),
		"pending_paths": pendingPaths, "next_retry_at": nextRetry,
	}
}

func earlierTime(first, second *time.Time) *time.Time {
	if first == nil {
		return second
	}
	if second == nil || first.Before(*second) {
		return first
	}
	return second
}

// --- Sondeo de respaldo -----------------------------------------------
//
// Se usa sólo cuando fsnotify no puede vigilar el vault (permisos, unidad de
// red sin soporte de notificaciones, límite de watches del sistema). Sigue
// pasando por el mismo debounce y el mismo IndexPaths dirigido: sólo cambia
// cómo se detectan los cambios, no cómo se reindexan.

type vaultFileStamp struct {
	Size    int64
	ModTime int64
}

func (w *VaultWatcher) pollLoop(ctx context.Context, generation int64) {
	previous, _, _ := scanVaultSnapshot(w.target(), nil)
	ticker := time.NewTicker(fallbackPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.generation.Load() != generation {
				return
			}
			current, changed, err := scanVaultSnapshot(w.target(), previous)
			if err != nil {
				w.setError("No se pudo vigilar el vault.")
				continue
			}
			if !changed {
				continue
			}
			for _, relative := range diffSnapshots(previous, current) {
				w.touch(relative)
			}
			previous = current
		}
	}
}

func scanVaultSnapshot(vault string, previous map[string]vaultFileStamp) (map[string]vaultFileStamp, bool, error) {
	files, err := DiscoverMarkdownFiles(vault)
	if err != nil {
		return nil, false, err
	}
	current := make(map[string]vaultFileStamp, len(files))
	changed := len(previous) != len(files)
	for _, path := range files {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, false, statErr
		}
		relative, relErr := filepath.Rel(vault, path)
		if relErr != nil {
			return nil, false, relErr
		}
		key := filepath.ToSlash(relative)
		stamp := vaultFileStamp{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		current[key] = stamp
		if !changed && previous[key] != stamp {
			changed = true
		}
	}
	return current, changed, nil
}

func diffSnapshots(previous, current map[string]vaultFileStamp) []string {
	touched := make(map[string]bool, len(previous)+len(current))
	for key, stamp := range current {
		if previous[key] != stamp {
			touched[key] = true
		}
	}
	for key := range previous {
		if _, ok := current[key]; !ok {
			touched[key] = true
		}
	}
	result := make([]string, 0, len(touched))
	for key := range touched {
		result = append(result, key)
	}
	return result
}

func nilIfBlank(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
