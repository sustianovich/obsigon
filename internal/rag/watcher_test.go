package rag

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// watcherIndexer es un indexRunner falso que registra cada lote dirigido y
// cada reconciliación completa, para comprobar qué le pide el watcher sin
// tocar SQLite.
type watcherIndexer struct {
	mu         sync.Mutex
	indexCalls int
	batches    [][2][]string
	indexErrors []error
	batchErrors []error
}

func (w *watcherIndexer) Index(context.Context) (IndexResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.indexCalls++
	if len(w.indexErrors) == 0 {
		return IndexResult{}, nil
	}
	err := w.indexErrors[0]
	w.indexErrors = w.indexErrors[1:]
	return IndexResult{}, err
}

func (w *watcherIndexer) IndexPaths(_ context.Context, changed, deleted []string, _ func(IndexProgressEvent)) (IndexResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.batches = append(w.batches, [2][]string{append([]string(nil), changed...), append([]string(nil), deleted...)})
	if len(w.batchErrors) == 0 {
		return IndexResult{}, nil
	}
	err := w.batchErrors[0]
	w.batchErrors = w.batchErrors[1:]
	return IndexResult{}, err
}

func (w *watcherIndexer) snapshot() ([][2][]string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][2][]string(nil), w.batches...), w.indexCalls
}

// waitForBackend espera a que el watcher reporte el backend dado y añade un
// pequeño margen adicional: en Windows, ReadDirectoryChangesW tarda un
// instante en armarse tras Add() y puede perderse un cambio hecho justo
// después. Ese margen es irrelevante en producción (las ediciones no llegan
// en milisegundos tras arrancar el watcher) pero evita que las pruebas sean
// inestables por esa latencia del sistema operativo.
func waitForBackend(t *testing.T, watcher *VaultWatcher, backend string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if watcher.Status()["backend"] == backend {
			time.Sleep(150 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend no llegó a %q: %+v", backend, watcher.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForBatch(t *testing.T, indexer *watcherIndexer) [][2][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		batches, _ := indexer.snapshot()
		if len(batches) > 0 {
			return batches
		}
		if time.Now().After(deadline) {
			t.Fatal("no llegó ningún lote dirigido")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestVaultWatcherDebouncesEditsIntoOneDirectedBatch(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "nota.md"), []byte("uno"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(vault, ".obsidian"), 0o700); err != nil {
		t.Fatal(err)
	}
	indexer := &watcherIndexer{}
	watcher := NewVaultWatcher(vault, indexer, 30*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	waitForBackend(t, watcher, "events")

	if err := os.WriteFile(filepath.Join(vault, ".obsidian", "workspace.md"), []byte("ignorado"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if batches, _ := indexer.snapshot(); len(batches) != 0 {
		t.Fatalf("el watcher indexó un cambio excluido: %+v", batches)
	}

	for _, value := range []string{"dos", "tres", "cuatro"} {
		if err := os.WriteFile(filepath.Join(vault, "nota.md"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	batches := waitForBatch(t, indexer)
	if len(batches) != 1 || len(batches[0][0]) != 1 || batches[0][0][0] != "nota.md" || len(batches[0][1]) != 0 {
		t.Fatalf("batches=%+v", batches)
	}
	if watcher.Status()["state"] != "up_to_date" {
		t.Fatalf("estado=%+v", watcher.Status())
	}
}

func TestVaultWatcherHandlesCreateAndDeleteAsOneBatch(t *testing.T) {
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "existente.md"), []byte("hola"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexer := &watcherIndexer{}
	watcher := NewVaultWatcher(vault, indexer, 30*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	waitForBackend(t, watcher, "events")

	if err := os.Remove(filepath.Join(vault, "existente.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "nueva.md"), []byte("nueva"), 0o600); err != nil {
		t.Fatal(err)
	}

	batches := waitForBatch(t, indexer)
	if len(batches) != 1 {
		t.Fatalf("batches=%+v", batches)
	}
	changed, deleted := batches[0][0], batches[0][1]
	if len(changed) != 1 || changed[0] != "nueva.md" || len(deleted) != 1 || deleted[0] != "existente.md" {
		t.Fatalf("changed=%v deleted=%v", changed, deleted)
	}
}

func TestVaultWatcherWatchesNewSubdirectories(t *testing.T) {
	vault := t.TempDir()
	indexer := &watcherIndexer{}
	watcher := NewVaultWatcher(vault, indexer, 30*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	waitForBackend(t, watcher, "events")

	sub := filepath.Join(vault, "carpeta-nueva")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "nota.md"), []byte("contenido"), 0o600); err != nil {
		t.Fatal(err)
	}

	batches := waitForBatch(t, indexer)
	found := false
	for _, batch := range batches {
		for _, path := range batch[0] {
			if path == "carpeta-nueva/nota.md" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no se vigiló el directorio nuevo: batches=%+v", batches)
	}
}

func TestVaultWatcherReconcilesPeriodically(t *testing.T) {
	vault := t.TempDir()
	indexer := &watcherIndexer{}
	watcher := NewVaultWatcher(vault, indexer, 20*time.Millisecond, 40*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, calls := indexer.snapshot(); calls >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("la reconciliación periódica no ejecutó Index")
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := watcher.Status()
	if status["last_reconciled_at"] == nil {
		t.Fatalf("estado=%+v", status)
	}
}

func TestVaultWatcherRetargetSwitchesVaultAndIndexer(t *testing.T) {
	vaultA := t.TempDir()
	vaultB := t.TempDir()
	indexerA := &watcherIndexer{}
	indexerB := &watcherIndexer{}
	watcher := NewVaultWatcher(vaultA, indexerA, 30*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)
	waitForBackend(t, watcher, "events")

	watcher.Retarget(vaultB, indexerB)
	waitForBackend(t, watcher, "events")

	if err := os.WriteFile(filepath.Join(vaultA, "viejo.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if batches, calls := indexerA.snapshot(); len(batches) != 0 || calls != 0 {
		t.Fatalf("el vault anterior siguió indexando tras Retarget: batches=%+v calls=%d", batches, calls)
	}

	if err := os.WriteFile(filepath.Join(vaultB, "nuevo.md"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	batches := waitForBatch(t, indexerB)
	if len(batches) != 1 || len(batches[0][0]) != 1 || batches[0][0][0] != "nuevo.md" {
		t.Fatalf("batches=%+v", batches)
	}
}
