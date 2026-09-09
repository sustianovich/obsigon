package rag

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"obsigon/internal/db"
)

func vectorDoc(path, title, estado string, tags []string, vectors ...[]float32) db.Document {
	chunks := make([]db.Chunk, len(vectors))
	for index, vector := range vectors {
		chunks[index] = db.Chunk{Index: index, Heading: "h", Content: path + " chunk", Embedding: vector}
	}
	return db.Document{
		Path: path, Title: title, SHA256: title, MTime: 1,
		Metadata: map[string]any{"estado": estado, "_tags": tags, "_vigencia": map[string]any{"estado_temporal": "vigente"}},
		Chunks:   chunks,
	}
}

func TestSearchAndGraphContinueDuringWrite(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "concurrent.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.ReplaceDocument(ctx, vectorDoc("a.md", "A", "vigente", nil, []float32{1, 0, 0})); err != nil {
		t.Fatal(err)
	}
	vectors := NewVectorStore(store, 4)
	if _, err := vectors.Search(ctx, []float32{1, 0, 0}, 3, -1, "", "", nil); err != nil {
		t.Fatal(err)
	}

	writer, err := store.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE documents SET sha256='uncommitted' WHERE path='a.md'`); err != nil {
		t.Fatal(err)
	}

	const goroutines = 12
	errors := make(chan error, goroutines)
	for index := range goroutines {
		go func(search bool) {
			if search {
				_, err := vectors.Search(ctx, []float32{1, 0, 0}, 3, -1, "", "", nil)
				errors <- err
				return
			}
			_, err := store.GraphStatus(ctx)
			errors <- err
		}(index%2 == 0)
	}
	for range goroutines {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatalf("lectura concurrente fallida: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("SearchDocuments o GraphStatus esperó a la transacción de escritura")
		}
	}
}

// TestIncrementalSnapshotMatchesFullReload comprueba que, tras altas, reemplazos
// y borrados, el snapshot reconstruido de forma incremental es idéntico al que
// produce una carga completa desde cero sobre la misma base.
func TestIncrementalSnapshotMatchesFullReload(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "vs.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	seed := []db.Document{
		vectorDoc("uno.md", "Uno", "aprobado", []string{"a", "a/b"}, []float32{1, 0, 0}, []float32{.9, .1, 0}),
		vectorDoc("dos.md", "Dos", "borrador", []string{"a"}, []float32{0, 1, 0}),
		vectorDoc("tres.md", "Tres", "aprobado", []string{"c"}, []float32{0, 0, 1}, []float32{.2, .2, .9}),
	}
	for _, document := range seed {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}

	incremental := NewVectorStore(store, 3)
	if _, err := incremental.Search(ctx, []float32{1, 0, 0}, 50, -1, "", "", nil); err != nil {
		t.Fatal(err)
	}

	// Alta de un documento, reemplazo de otro (embedding y tags nuevos) y borrado
	// de un tercero. Cada operación crea ids de fragmento nuevos.
	if err := store.ReplaceDocument(ctx, vectorDoc("cuatro.md", "Cuatro", "aprobado", []string{"a", "d"}, []float32{.7, .7, 0})); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDocument(ctx, vectorDoc("uno.md", "Uno", "aprobado", []string{"a", "z"}, []float32{.5, .5, .5})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteDocumentsNotIn(ctx, map[string]bool{"uno.md": true, "tres.md": true, "cuatro.md": true}); err != nil {
		t.Fatal(err)
	}

	queries := [][]float32{{1, 0, 0}, {0, 1, 0}, {.3, .3, .3}}
	filters := []struct {
		status, vigencia string
		tags             []string
	}{
		{"", "", nil},
		{"aprobado", "no_caducada", nil},
		{"", "", []string{"a"}},
	}
	for _, query := range queries {
		for _, filter := range filters {
			got, err := incremental.Search(ctx, query, 50, -1, filter.status, filter.vigencia, filter.tags)
			if err != nil {
				t.Fatal(err)
			}
			fresh := NewVectorStore(store, 3)
			want, err := fresh.Search(ctx, query, 50, -1, filter.status, filter.vigencia, filter.tags)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("query=%v filtro=%+v\n full=%+v\n incr=%+v", query, filter, want, got)
			}
		}
	}

	incrementalStats, err := incremental.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	freshStats, err := NewVectorStore(store, 3).Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshStats, incrementalStats) {
		t.Fatalf("stats divergen\n full=%+v\n incr=%+v", freshStats, incrementalStats)
	}
}

// TestIncrementalSnapshotFullReindexFallback cubre el caso en que ninguna fila
// anterior sobrevive (reindexación completa): el snapshot debe recaer en la
// carga total y seguir siendo correcto.
func TestIncrementalSnapshotFullReindexFallback(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "vs.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.ReplaceDocument(ctx, vectorDoc("a.md", "A", "aprobado", []string{"t"}, []float32{1, 0, 0})); err != nil {
		t.Fatal(err)
	}
	vectors := NewVectorStore(store, 2)
	if _, err := vectors.Search(ctx, []float32{1, 0, 0}, 10, -1, "", "", nil); err != nil {
		t.Fatal(err)
	}
	// Reemplaza todos los documentos: los ids de fragmento antiguos desaparecen.
	if err := store.ReplaceDocument(ctx, vectorDoc("a.md", "A", "aprobado", []string{"t"}, []float32{0, 1, 0})); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDocument(ctx, vectorDoc("b.md", "B", "aprobado", []string{"t"}, []float32{0, 0, 1})); err != nil {
		t.Fatal(err)
	}
	got, err := vectors.Search(ctx, []float32{0, 1, 0}, 10, -1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := NewVectorStore(store, 2).Search(ctx, []float32{0, 1, 0}, 10, -1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("full=%+v incr=%+v", want, got)
	}
	if len(got) != 2 {
		t.Fatalf("esperaba 2 filas tras la reindexación, hay %d", len(got))
	}
}
