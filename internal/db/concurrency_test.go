package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrentReadersDoNotWaitForWriter(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.ReplaceDocument(ctx, Document{
		Path:   "nota.md",
		Title:  "Nota",
		SHA256: "original",
		Chunks: []Chunk{{Index: 0, Content: "contenido", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}

	writer, err := store.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE documents SET sha256='pendiente' WHERE path='nota.md'`); err != nil {
		t.Fatal(err)
	}

	const readers = 12
	errors := make(chan error, readers)
	var ready sync.WaitGroup
	ready.Add(readers)
	for range readers {
		go func() {
			ready.Done()
			readCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			defer cancel()
			var hash string
			errors <- store.ReadDB().QueryRowContext(readCtx, `SELECT sha256 FROM documents WHERE path='nota.md'`).Scan(&hash)
		}()
	}
	ready.Wait()
	for range readers {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatalf("una lectura se bloqueó durante la escritura: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("las lecturas no avanzaron mientras había una escritura abierta")
		}
	}
}

func TestReadPoolIsQueryOnly(t *testing.T) {
	store := testStore(t)
	if _, err := store.ReadDB().Exec(`INSERT INTO app_meta(key,value) VALUES('unexpected','write')`); err == nil {
		t.Fatal("el pool de lectura permitió una escritura")
	}
}
