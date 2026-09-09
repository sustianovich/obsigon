package rag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

type fakeEmbedder struct{ fail bool }

func (f *fakeEmbedder) Dimension(context.Context) (int, error) { return 2, nil }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.fail {
		return nil, errors.New("fallo simulado")
	}
	vectors := make([][]float32, len(texts))
	for index := range texts {
		vectors[index] = []float32{float32(index + 1), 1}
	}
	return vectors, nil
}

func testIndexer(t *testing.T) (*Indexer, string) {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(root, "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := config.Settings{VaultPath: vault, DatabasePath: filepath.Join(root, "index.sqlite3"), EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto", ChunkSize: 1800, ChunkOverlap: 250, ParentChunkSize: 6000, ChildChunkSize: 700, ChildChunkOverlap: 100}
	return &Indexer{Settings: settings, Store: store, Embedder: &fakeEmbedder{}}, vault
}

func TestDiscoveryExcludesHiddenObsidianAndTemplates(t *testing.T) {
	root := t.TempDir()
	paths := []string{"notas/Visible.md", ".privado/Oculta.md", ".obsidian/Config.md", "_plantillas/Plantilla.md", "_templates/Template.md"}
	for _, relative := range paths {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# Nota"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := DiscoverMarkdownFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "Visible.md" {
		t.Fatalf("descubiertos=%v", files)
	}
}

func TestIndexIsAtomicThenIncrementalBySHA256(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	if err := os.WriteFile(filepath.Join(vault, "Origen.md"), []byte("# Origen\n\nContenido [[Destino]]."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "Destino.md"), []byte("# Destino\n\nDato clínico."), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := indexer.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Indexed != 2 || first.Unchanged != 0 || first.Stats.Documents != 2 || first.Stats.Chunks != 2 || first.Stats.Links != 1 {
		t.Fatalf("primera=%+v", first)
	}
	second, err := indexer.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Indexed != 0 || second.Unchanged != 2 {
		t.Fatalf("segunda=%+v", second)
	}
	if err := os.WriteFile(filepath.Join(vault, "Destino.md"), []byte("# Destino\n\nDato clínico actualizado."), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := indexer.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Indexed != 1 || third.Unchanged != 1 {
		t.Fatalf("tercera=%+v", third)
	}
}

func TestIndexerPublishesRealProgress(t *testing.T) {
	indexer, vault := testIndexer(t)
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte("# Nota\n\nContenido."), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	events := []IndexProgressEvent{}
	result, err := indexer.IndexWithProgress(context.Background(), func(event IndexProgressEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Indexed != 2 || len(events) < 4 || events[0].Phase != "scanning" {
		t.Fatalf("result=%+v events=%+v", result, events)
	}
	last := events[len(events)-1]
	if last.Phase != "committing" || last.Processed != 2 || last.Total != 2 {
		t.Fatalf("last=%+v", last)
	}
}

func TestFailedRebuildKeepsPreviousFingerprintAndDocuments(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	path := filepath.Join(vault, "Nota.md")
	if err := os.WriteFile(path, []byte("# Nota\n\nContenido."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}
	before, ok, err := indexer.Store.Meta(ctx, IndexFingerprintKey)
	if err != nil || !ok {
		t.Fatalf("huella inicial: %q %v %v", before, ok, err)
	}
	indexer.Settings.ChunkSize = 1900
	indexer.Embedder = &fakeEmbedder{fail: true}
	result, err := indexer.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("la reconstrucción fallida no informó error")
	}
	after, _, err := indexer.Store.Meta(ctx, IndexFingerprintKey)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("la huella cambió: antes=%s después=%s", before, after)
	}
	stats, err := indexer.Store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 1 {
		t.Fatalf("se perdió el índice: %+v", stats)
	}
}

// countingEmbedder cuenta cuántos documentos pasaron por Embed, para
// comprobar que IndexPaths no toca archivos fuera del lote dirigido.
type countingEmbedder struct {
	fakeEmbedder
	calls int
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	return c.fakeEmbedder.Embed(ctx, texts)
}

func TestIndexPathsOnlyTouchesTheGivenBatch(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	for _, name := range []string{"Uno.md", "Dos.md", "Tres.md"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte("# "+name+"\n\nContenido."), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}
	counting := &countingEmbedder{}
	indexer.Embedder = counting

	if err := os.WriteFile(filepath.Join(vault, "Dos.md"), []byte("# Dos\n\nContenido actualizado."), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := indexer.IndexPaths(ctx, []string{"Dos.md"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Indexed != 1 || result.Scanned != 1 || counting.calls != 1 {
		t.Fatalf("resultado=%+v llamadas_embed=%d", result, counting.calls)
	}
	stats, err := indexer.Store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 3 {
		t.Fatalf("se perdieron documentos fuera del lote: %+v", stats)
	}
}

func TestIndexPathsDeletesGivenPaths(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	for _, name := range []string{"Uno.md", "Dos.md"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte("# "+name+"\n\nContenido."), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(vault, "Dos.md")); err != nil {
		t.Fatal(err)
	}
	result, err := indexer.IndexPaths(ctx, nil, []string{"Dos.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("resultado=%+v", result)
	}
	stats, err := indexer.Store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 1 {
		t.Fatalf("no se borró el documento: %+v", stats)
	}
}

func TestIndexPathsFallsBackToFullRebuildWhenConfigurationChanged(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	if err := os.WriteFile(filepath.Join(vault, "Uno.md"), []byte("# Uno\n\nContenido."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}
	indexer.Settings.ChunkSize = 1900
	if err := os.WriteFile(filepath.Join(vault, "Dos.md"), []byte("# Dos\n\nContenido."), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := indexer.IndexPaths(ctx, []string{"Dos.md"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rebuilt || result.Indexed != 2 {
		t.Fatalf("no delegó en una reconstrucción completa: %+v", result)
	}
}

// TestIndexPathsCostIsProportionalToTheBatchNotTheCorpus reproduce a escala
// (miles de notas) el criterio de aceptación central de la reindexación
// dirigida: editar una nota no debe leer, hashear ni embeber el resto del
// vault. countingEmbedder cuenta llamadas a Embed, que sólo ocurren por
// documento tocado.
func TestIndexPathsCostIsProportionalToTheBatchNotTheCorpus(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	const corpusSize = 3000
	for index := 0; index < corpusSize; index++ {
		name := fmt.Sprintf("nota-%04d.md", index)
		if err := os.WriteFile(filepath.Join(vault, name), []byte(fmt.Sprintf("# Nota %d\n\nContenido inicial.", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}

	counting := &countingEmbedder{}
	indexer.Embedder = counting
	touched := "nota-1500.md"
	if err := os.WriteFile(filepath.Join(vault, touched), []byte("# Nota 1500\n\nContenido actualizado."), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := indexer.IndexPaths(ctx, []string{touched}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Indexed != 1 || result.Scanned != 1 || counting.calls != 1 {
		t.Fatalf("el coste no fue proporcional al lote: resultado=%+v llamadas_embed=%d (corpus=%d)", result, counting.calls, corpusSize)
	}
	stats, err := indexer.Store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != corpusSize {
		t.Fatalf("se perdieron documentos del corpus: %+v", stats)
	}
}

func TestIndexPathsTreatsMissingFileAsDeleted(t *testing.T) {
	ctx := context.Background()
	indexer, vault := testIndexer(t)
	if err := os.WriteFile(filepath.Join(vault, "Uno.md"), []byte("# Uno\n\nContenido."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.Index(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(vault, "Uno.md")); err != nil {
		t.Fatal(err)
	}
	// El evento de borrado se perdió y sólo llegó como "cambiado"; IndexPaths
	// debe comprobar el disco y tratarlo como borrado en vez de fallar.
	result, err := indexer.IndexPaths(ctx, []string{"Uno.md"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || len(result.Errors) != 0 {
		t.Fatalf("resultado=%+v", result)
	}
}
