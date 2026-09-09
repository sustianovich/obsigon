package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesVersionedSchemaWithoutBackingUpNewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.LastMigrationBackup() != "" {
		t.Fatalf("una base nueva no debe crear backup: %q", store.LastMigrationBackup())
	}
	version, err := databaseSchemaVersion(context.Background(), store.ReadDB())
	if err != nil || version != LatestSchemaVersion {
		t.Fatalf("versión=%d err=%v", version, err)
	}
}

func TestOpenBacksUpLegacyDatabaseBeforeMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.ReplaceDocument(ctx, Document{
		Path: "nota.md", Title: "Dato conservado", SHA256: "legacy",
		Metadata: map[string]any{},
		Chunks:   []Chunk{{Index: 0, Content: "contenido", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.WriteDB().ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	backup := migrated.LastMigrationBackup()
	if backup == "" {
		t.Fatal("la base existente sin versión debía generar un backup")
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Fatalf("backup=%q info=%v err=%v", backup, info, err)
	}

	backupDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(backup))
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	var version int
	if err := backupDB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 0 {
		t.Fatalf("versión del backup=%d err=%v", version, err)
	}
	var title string
	if err := backupDB.QueryRowContext(ctx, `SELECT title FROM documents WHERE path='nota.md'`).Scan(&title); err != nil || title != "Dato conservado" {
		t.Fatalf("dato del backup=%q err=%v", title, err)
	}
}

func TestOpenRebuildsFTSOnlyWhenVersionOrCountsRequireIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fts.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDocument(ctx, Document{
		Path: "nota.md", Title: "Nota", SHA256: "one", Metadata: map[string]any{},
		Chunks: []Chunk{{Index: 0, Content: "contenido original", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteDB().ExecContext(ctx, `UPDATE chunks_fts SET content='sentinela'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	unchanged, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	if err := unchanged.DB.QueryRowContext(ctx, `SELECT content FROM chunks_fts`).Scan(&content); err != nil || content != "sentinela" {
		t.Fatalf("FTS sincronizado no debía reconstruirse: contenido=%q err=%v", content, err)
	}
	if _, err := unchanged.WriteDB().ExecContext(ctx, `UPDATE app_meta SET value='obsoleta' WHERE key=?`, FTSVersionMetaKey); err != nil {
		t.Fatal(err)
	}
	if err := unchanged.Close(); err != nil {
		t.Fatal(err)
	}

	rebuilt, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if err := rebuilt.DB.QueryRowContext(ctx, `SELECT content FROM chunks_fts`).Scan(&content); err != nil || content != "contenido original" {
		t.Fatalf("FTS obsoleto no se reconstruyó: contenido=%q err=%v", content, err)
	}
}
