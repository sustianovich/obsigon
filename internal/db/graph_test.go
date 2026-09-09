package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

func TestGraphStatusCountsResolvedBrokenOrphansAndAmbiguousAliases(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "status.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, document := range []Document{
		{Path: "origen.md", Title: "Origen", SHA256: "1", Links: []Link{{Target: "Destino"}, {Target: "Rota"}}, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "destino.md", Title: "Destino", SHA256: "2", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "huerfana.md", Title: "Huérfana", SHA256: "3", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "a/Index.md", Title: "Índice", SHA256: "4", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "b/Index.md", Title: "Índice", SHA256: "5", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
	} {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.GraphStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalEdges != 2 || status.ResolvedEdges != 1 || status.BrokenEdges != 1 || status.OrphanDocuments != 3 || status.AmbiguousAliases < 1 {
		t.Fatalf("status=%+v", status)
	}
}

func TestBacklinkCountsDistinctSources(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "backlinks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, document := range []Document{
		{Path: "hub.md", Title: "Hub", SHA256: "h", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "a.md", Title: "A", SHA256: "a", Links: []Link{{Target: "Hub"}, {Target: "Hub", Section: "Dos"}}, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
		{Path: "b.md", Title: "B", SHA256: "b", Links: []Link{{Target: "Hub"}}, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}},
	} {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	var hubID int64
	if err := store.ReadDB().QueryRowContext(ctx, "SELECT id FROM documents WHERE path='hub.md'").Scan(&hubID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.BacklinkCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[hubID] != 2 {
		t.Fatalf("backlinks=%v, want 2 fuentes distintas", counts)
	}
}

func TestIncrementalGraphHandlesAliasShadowingAndDeletion(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	source := Document{Path: "source.md", Title: "Source", SHA256: "source", Links: []Link{{Target: "Shared"}}, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
	shadow := Document{Path: "z/second.md", Title: "Shared", SHA256: "shadow", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
	winner := Document{Path: "a/first.md", Title: "Shared", SHA256: "winner", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}

	for _, document := range []Document{source, shadow, winner} {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	assertLinkTarget(t, store, "a/first.md")

	if _, err := store.DeleteDocumentsNotIn(ctx, map[string]bool{"source.md": true, "z/second.md": true}); err != nil {
		t.Fatal(err)
	}
	assertLinkTarget(t, store, "z/second.md")

	if err := store.ReplaceDocument(ctx, winner); err != nil {
		t.Fatal(err)
	}
	assertLinkTarget(t, store, "a/first.md")
	winner.Title = "Other"
	winner.SHA256 = "renamed"
	if err := store.ReplaceDocument(ctx, winner); err != nil {
		t.Fatal(err)
	}
	assertLinkTarget(t, store, "z/second.md")

	if _, err := store.DeleteDocumentsNotIn(ctx, map[string]bool{"source.md": true, "a/first.md": true}); err != nil {
		t.Fatal(err)
	}
	assertLinkTarget(t, store, "")

	if store.graphIndexBuilds != 1 {
		t.Fatalf("el índice incremental se reconstruyó %d veces; se esperaba una", store.graphIndexBuilds)
	}
	tx, err := store.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuildLinks(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertLinkTarget(t, store, "")
}

func TestDeleteDocumentsRemovesExactlyTheGivenPaths(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	source := Document{Path: "source.md", Title: "Source", SHA256: "source", Links: []Link{{Target: "Shared"}}, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
	shadow := Document{Path: "z/second.md", Title: "Shared", SHA256: "shadow", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
	winner := Document{Path: "a/first.md", Title: "Shared", SHA256: "winner", Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
	for _, document := range []Document{source, shadow, winner} {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	assertLinkTarget(t, store, "a/first.md")

	removed, err := store.DeleteDocuments(ctx, []string{"a/first.md", "no-existe.md"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("borrados=%d, se esperaba 1 (la ruta inexistente no cuenta)", removed)
	}
	assertLinkTarget(t, store, "z/second.md")

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 2 {
		t.Fatalf("stats=%+v, se esperaban 2 documentos restantes", stats)
	}

	if removed, err := store.DeleteDocuments(ctx, nil); err != nil || removed != 0 {
		t.Fatalf("una lista vacía no debe borrar nada: removed=%d err=%v", removed, err)
	}
}

func assertLinkTarget(t *testing.T, store *Store, want string) {
	t.Helper()
	var target sql.NullString
	err := store.ReadDB().QueryRow(`SELECT target.path FROM document_links link LEFT JOIN documents target ON target.id=link.target_document_id JOIN documents source ON source.id=link.source_document_id WHERE source.path='source.md'`).Scan(&target)
	if err != nil {
		t.Fatal(err)
	}
	if target.String != want || target.Valid != (want != "") {
		t.Fatalf("destino=%q (válido=%t), esperado=%q", target.String, target.Valid, want)
	}
}

func TestRandomIncrementalGraphMatchesFullRebuild(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	random := rand.New(rand.NewSource(20260902))
	paths := make([]string, 18)
	for index := range paths {
		paths[index] = fmt.Sprintf("folder-%d/note-%02d.md", index%4, index)
	}
	titles := []string{"Shared", "Plan", "Index", "Decision", "Unique"}
	active := map[string]bool{}
	for step := 0; step < 80; step++ {
		if len(active) == 0 || random.Intn(4) != 0 {
			path := paths[random.Intn(len(paths))]
			linkCount := random.Intn(4)
			links := make([]Link, linkCount)
			for linkIndex := range links {
				target := titles[random.Intn(len(titles))]
				if random.Intn(5) == 0 {
					target = "missing"
				}
				links[linkIndex] = Link{Target: target, Section: fmt.Sprintf("s%d", linkIndex), Embedded: random.Intn(2) == 0}
			}
			document := Document{Path: path, Title: titles[random.Intn(len(titles))], SHA256: fmt.Sprintf("%d", step), Links: links, Chunks: []Chunk{{Index: 0, Embedding: []float32{1}}}}
			if err := store.ReplaceDocument(ctx, document); err != nil {
				t.Fatalf("replace paso %d: %v", step, err)
			}
			active[path] = true
		} else {
			keep := map[string]bool{}
			for path := range active {
				if random.Intn(4) != 0 {
					keep[path] = true
				}
			}
			if _, err := store.DeleteDocumentsNotIn(ctx, keep); err != nil {
				t.Fatalf("delete paso %d: %v", step, err)
			}
			active = keep
		}

		query := `SELECT source.path,COALESCE(target.path,''),link.target_raw,link.section,link.embedded FROM document_links link JOIN documents source ON source.id=link.source_document_id LEFT JOIN documents target ON target.id=link.target_document_id ORDER BY 1,2,3,4,5`
		incremental := queryRowsJSON(t, store, query)
		tx, err := store.WriteDB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rebuildLinks(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		full := queryRowsJSON(t, store, query)
		if incremental != full {
			t.Fatalf("grafo distinto en paso %d\nincremental=%s\nfull=%s", step, incremental, full)
		}
	}
	if store.graphIndexBuilds != 1 {
		t.Fatalf("el índice de alias se reconstruyó %d veces", store.graphIndexBuilds)
	}
}
