package rag

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"obsigon/internal/db"
)

func TestHNSWRecallAtTen(t *testing.T) {
	const (
		count     = 1500
		dimension = 32
		queries   = 80
		topK      = 10
	)
	random := rand.New(rand.NewSource(42))
	rows := make([]vectorRow, count)
	rowByChunk := make(map[int64]int, count)
	for rowIndex := range rows {
		vector := make([]float32, dimension)
		for offset := range vector {
			vector[offset] = random.Float32() - .5
		}
		normalizeVector(vector)
		rows[rowIndex] = vectorRow{chunkID: int64(rowIndex + 1), documentID: int64(rowIndex + 1), vector: vector}
		rowByChunk[int64(rowIndex+1)] = rowIndex
	}
	index := buildHNSW(rows, dimension, 16, 200)
	exact := &VectorStore{workers: runtime.GOMAXPROCS(0)}
	matched := 0
	for range queries {
		query := make([]float32, dimension)
		for offset := range query {
			query[offset] = random.Float32() - .5
		}
		normalizeVector(query)
		want := exact.rankRows(rows, query, topK, -1, "", "", nil, nil)
		got := rankANN(index, rows, rowByChunk, query, topK, -1, "", "", nil, nil, 160)
		wanted := map[int64]struct{}{}
		for _, candidate := range want {
			wanted[candidate.ChunkID] = struct{}{}
		}
		for _, candidate := range got {
			if _, exists := wanted[candidate.ChunkID]; exists {
				matched++
			}
		}
	}
	recall := float64(matched) / float64(queries*topK)
	if recall < .98 {
		t.Fatalf("recall@10=%.3f; se esperaba al menos 0.98", recall)
	}
}

func TestANNPersistenceIncrementalPatchAndCorruptFallback(t *testing.T) {
	directory := t.TempDir()
	store, err := db.Open(filepath.Join(directory, "vectors.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, document := range []db.Document{
		vectorDoc("a.md", "A", "vigente", []string{"uno"}, []float32{1, 0, 0}),
		vectorDoc("b.md", "B", "vigente", []string{"dos"}, []float32{0, 1, 0}),
		vectorDoc("c.md", "C", "vigente", []string{"tres"}, []float32{0, 0, 1}),
	} {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(directory, "vectors.hnsw")
	config := ANNConfig{Enabled: true, Path: path, M: 8, EFConstruction: 64, EFSearch: 32}
	vectors := NewVectorStore(store, 2, config)
	first, backend, err := vectors.SearchDocumentsWithBackend(ctx, []float32{1, 0, 0}, 3, -1, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backend != "ann" || len(first) != 3 {
		t.Fatalf("backend=%q candidatos=%d", backend, len(first))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no se persistió el índice ANN: %v", err)
	}

	if err := store.ReplaceDocument(ctx, vectorDoc("d.md", "D", "vigente", nil, []float32{.9, .1, 0})); err != nil {
		t.Fatal(err)
	}
	vectors.Invalidate()
	patched, backend, err := vectors.SearchDocumentsWithBackend(ctx, []float32{1, 0, 0}, 4, -1, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backend != "ann" || len(patched) != 4 {
		t.Fatalf("el parche incremental no quedó disponible: backend=%q candidatos=%d", backend, len(patched))
	}

	reloaded := NewVectorStore(store, 2, config)
	_, backend, err = reloaded.SearchDocumentsWithBackend(ctx, []float32{1, 0, 0}, 4, -1, "", "", nil, nil)
	if err != nil || backend != "ann" {
		t.Fatalf("no se reutilizó el índice persistido: backend=%q err=%v", backend, err)
	}
	if err := os.WriteFile(path, []byte("corrupto"), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback := NewVectorStore(store, 2, config)
	results, backend, err := fallback.SearchDocumentsWithBackend(ctx, []float32{1, 0, 0}, 4, -1, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backend != "exact" || len(results) != 4 {
		t.Fatalf("fallback=%q candidatos=%d", backend, len(results))
	}
	exact, err := NewVectorStore(store, 2).Search(ctx, []float32{1, 0, 0}, 4, -1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exact, results) {
		t.Fatalf("el fallback exacto cambió el resultado\nwant=%+v\ngot=%+v", exact, results)
	}
	stats, err := fallback.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ANNAvailable || stats.ANNError == "" {
		t.Fatalf("estado ANN inesperado: %+v", stats)
	}
}
