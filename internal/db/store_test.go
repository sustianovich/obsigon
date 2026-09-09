package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDirectedIncrementalMatchesFullFTSAndGraphState(t *testing.T) {
	ctx := context.Background()
	old := Document{Path: "a.md", Title: "Nota A", SHA256: "old", Metadata: map[string]any{"estado": "borrador"}, Links: []Link{{Target: "Nota B"}}, Chunks: []Chunk{{Index: 0, Heading: "Antes", Content: "contenido anterior", Embedding: []float32{1, 0}}}}
	updated := Document{Path: "a.md", Title: "Nota A final", SHA256: "new", Metadata: map[string]any{"estado": "vigente"}, Links: []Link{{Target: "Nota C", Section: "Detalle"}}, Chunks: []Chunk{{Index: 0, Heading: "Después", Content: "contenido definitivo", Embedding: []float32{1, 0}}}}
	b := Document{Path: "b.md", Title: "Nota B", SHA256: "b", Metadata: map[string]any{}, Links: []Link{{Target: "Nota A final"}}, Chunks: []Chunk{{Index: 0, Content: "b", Embedding: []float32{0, 1}}}}
	c := Document{Path: "c.md", Title: "Nota C", SHA256: "c", Metadata: map[string]any{}, Chunks: []Chunk{{Index: 0, Content: "c", Embedding: []float32{.5, .5}}}}

	full, err := Open(filepath.Join(t.TempDir(), "full.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	runID := "full"
	for _, document := range []Document{updated, b, c} {
		if err := full.StageDocument(ctx, runID, document); err != nil {
			t.Fatal(err)
		}
	}
	links, err := full.ResolveStagedLinks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := full.CommitStagedIndex(ctx, runID, "fingerprint", "full", Stats{Documents: 3, Chunks: 3, Links: links}); err != nil {
		t.Fatal(err)
	}

	incremental, err := Open(filepath.Join(t.TempDir(), "incremental.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer incremental.Close()
	for _, document := range []Document{old, b, c} {
		if err := incremental.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	if err := incremental.ReplaceDocument(ctx, updated); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		`SELECT title,heading,content,path,document_status FROM chunks_fts ORDER BY path,heading`,
		`SELECT source.path,COALESCE(target.path,''),l.target_raw,l.section,l.embedded FROM document_links l JOIN documents source ON source.id=l.source_document_id LEFT JOIN documents target ON target.id=l.target_document_id ORDER BY source.path,l.rowid`,
		`SELECT run_id,source_document_path,COALESCE(target_document_path,''),target_raw,section,embedded FROM staged_document_links ORDER BY rowid`,
	} {
		left := queryRowsJSON(t, full, query)
		right := queryRowsJSON(t, incremental, query)
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("estado distinto para %s\nfull=%s\nincremental=%s", query, left, right)
		}
	}
}

func queryRowsJSON(t *testing.T, store *Store, query string) string {
	t.Helper()
	rows, err := store.ReadDB().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	result := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func testChunk(parent *int) []Chunk {
	return []Chunk{{Index: 0, Heading: "Documento", Content: "Contenido", ParentIndex: parent, Embedding: []float32{1, 0}}}
}

func TestAtomicPublicationActivatesDocumentsChunksFTSAndGraph(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	source := Document{Path: "origen.md", Title: "Origen", SHA256: "1", Links: []Link{{Target: "Destino", Section: "Apartado"}}, Chunks: testChunk(nil)}
	target := Document{Path: "destino.md", Title: "Destino", SHA256: "2", Chunks: testChunk(nil)}
	if err := store.StageDocument(ctx, "correcto", source); err != nil {
		t.Fatal(err)
	}
	if err := store.StageDocument(ctx, "correcto", target); err != nil {
		t.Fatal(err)
	}
	links, err := store.ResolveStagedLinks(ctx, "correcto")
	if err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("links=%d", links)
	}
	want := Stats{Documents: 2, Chunks: 2, Links: 1}
	if err := store.CommitStagedIndex(ctx, "correcto", "index_fingerprint", "correcto", want); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats != want {
		t.Fatalf("stats=%+v want=%+v", stats, want)
	}
	var sourcePath, targetPath, section string
	err = store.ReadDB().QueryRowContext(ctx, `SELECT source.path,target.path,link.section FROM document_links link JOIN documents source ON source.id=link.source_document_id JOIN documents target ON target.id=link.target_document_id`).Scan(&sourcePath, &targetPath, &section)
	if err != nil {
		t.Fatal(err)
	}
	if sourcePath != "origen.md" || targetPath != "destino.md" || section != "Apartado" {
		t.Fatalf("arista=%s,%s,%s", sourcePath, targetPath, section)
	}
	var fts int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks_fts`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if fts != 2 {
		t.Fatalf("fts=%d", fts)
	}
}

func TestIncompleteStagingKeepsPreviousIndex(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	old := Document{Path: "anterior.md", Title: "Anterior", SHA256: "old", Chunks: testChunk(nil)}
	if err := store.ReplaceDocument(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.StageDocument(ctx, "fallido", Document{Path: "nuevo.md", Title: "Nuevo", SHA256: "new", Chunks: testChunk(nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveStagedLinks(ctx, "fallido"); err != nil {
		t.Fatal(err)
	}
	err := store.CommitStagedIndex(ctx, "fallido", "index_fingerprint", "nuevo", Stats{Documents: 1, Chunks: 1, Links: 1})
	if err == nil {
		t.Fatal("se aceptó un staging incompleto")
	}
	var path string
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT path FROM documents`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "anterior.md" {
		t.Fatalf("se alteró el índice anterior: %s", path)
	}
}

func TestParentChildRelationshipSurvivesAtomicCommit(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	parentIndex := 0
	doc := Document{Path: "nota.md", Title: "Nota", SHA256: "x", Parents: []Parent{{Index: 0, Heading: "Tema", Content: "Padre completo"}}, Chunks: testChunk(&parentIndex)}
	if err := store.StageDocument(ctx, "parent", doc); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveStagedLinks(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitStagedIndex(ctx, "parent", "index_fingerprint", "parent", Stats{Documents: 1, Chunks: 1, Parents: 1}); err != nil {
		t.Fatal(err)
	}
	var content string
	var parentID int64
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT parent.content,child.parent_id FROM chunks child JOIN chunk_parents parent ON parent.id=child.parent_id`).Scan(&content, &parentID); err != nil {
		t.Fatal(err)
	}
	if content != "Padre completo" || parentID == 0 {
		t.Fatalf("relación inválida: %q %d", content, parentID)
	}
}
