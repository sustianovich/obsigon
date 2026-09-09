// Package db implementa el índice local SQLite sin CGO.
package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultProjectID    = "default"
	DefaultProjectName  = "General"
	FTSIndexVersion     = "1"
	GraphIndexVersion   = "1"
	FTSVersionMetaKey   = "fts_index_version"
	GraphVersionMetaKey = "document_links_version"
	LatestSchemaVersion = 1
)

type Store struct {
	// DB se conserva como alias de solo lectura por compatibilidad. El código
	// nuevo debe usar ReadDB y WriteDB para hacer explícita la intención.
	DB                  *sql.DB
	writeDB             *sql.DB
	path                string
	lastMigrationBackup string
	graphMu             sync.Mutex
	graphIndex          *activeGraphIndex
	graphIndexBuilds    int
}

type Parent struct {
	Index            int
	Heading, Content string
}
type Chunk struct {
	Index            int
	Heading, Content string
	ParentIndex      *int
	Embedding        []float32
}
type Link struct {
	Target, Section, Alias string
	Embedded               bool
}
type Document struct {
	Path, Title, SHA256 string
	MTime               float64
	Metadata            map[string]any
	Links               []Link
	Parents             []Parent
	Chunks              []Chunk
}
type Stats struct {
	Documents int `json:"documents"`
	Chunks    int `json:"chunks"`
	Parents   int `json:"parents"`
	Links     int `json:"links"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("no se pudo crear el directorio de la base de datos: %w", err)
	}
	base := "file:" + filepath.ToSlash(path)
	writeDSN := base + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_txlock=immediate"
	writer, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	store := &Store{writeDB: writer, path: path}
	if err := store.Init(context.Background()); err != nil {
		writer.Close()
		return nil, err
	}
	readDSN := base + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)&_pragma=query_only(1)"
	reader, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writer.Close()
		return nil, err
	}
	readers := max(2, runtime.GOMAXPROCS(0))
	reader.SetMaxOpenConns(readers)
	reader.SetMaxIdleConns(readers)
	if err := reader.PingContext(context.Background()); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	store.DB = reader
	return store, nil
}

func (s *Store) ReadDB() *sql.DB  { return s.DB }
func (s *Store) WriteDB() *sql.DB { return s.writeDB }

func (s *Store) Close() error {
	var readErr, writeErr error
	if s.DB != nil {
		readErr = s.DB.Close()
	}
	if s.writeDB != nil {
		writeErr = s.writeDB.Close()
	}
	return errors.Join(readErr, writeErr)
}

func (s *Store) Init(ctx context.Context) error {
	version, err := databaseSchemaVersion(ctx, s.WriteDB())
	if err != nil {
		return fmt.Errorf("no se pudo leer la versión del esquema SQLite: %w", err)
	}
	if version > LatestSchemaVersion {
		return fmt.Errorf("la base de datos usa el esquema %d, pero esta versión de Obsigon solo admite hasta %d", version, LatestSchemaVersion)
	}
	if version < LatestSchemaVersion {
		hasSchema, schemaErr := databaseHasUserSchema(ctx, s.WriteDB())
		if schemaErr != nil {
			return fmt.Errorf("no se pudo inspeccionar el esquema SQLite: %w", schemaErr)
		}
		if hasSchema {
			backup, backupErr := s.backupBeforeMigration(ctx, version, LatestSchemaVersion)
			if backupErr != nil {
				return fmt.Errorf("no se pudo crear la copia previa a la migración: %w", backupErr)
			}
			s.lastMigrationBackup = backup
		}
		if err := s.applyMigrations(ctx, version); err != nil {
			return err
		}
	}
	return s.ensureDerivedState(ctx)
}

func schemaV1Statements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS documents (id INTEGER PRIMARY KEY AUTOINCREMENT,path TEXT NOT NULL UNIQUE,title TEXT NOT NULL,sha256 TEXT NOT NULL,mtime REAL NOT NULL,metadata_json TEXT NOT NULL,links_json TEXT NOT NULL,indexed_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chunk_parents (id INTEGER PRIMARY KEY AUTOINCREMENT,document_id INTEGER NOT NULL,parent_index INTEGER NOT NULL,heading TEXT NOT NULL,content TEXT NOT NULL,FOREIGN KEY(document_id) REFERENCES documents(id) ON DELETE CASCADE,UNIQUE(document_id,parent_index))`,
		`CREATE TABLE IF NOT EXISTS chunks (id INTEGER PRIMARY KEY AUTOINCREMENT,document_id INTEGER NOT NULL,chunk_index INTEGER NOT NULL,heading TEXT NOT NULL,content TEXT NOT NULL,parent_id INTEGER,embedding BLOB NOT NULL,embedding_dim INTEGER NOT NULL,FOREIGN KEY(document_id) REFERENCES documents(id) ON DELETE CASCADE,FOREIGN KEY(parent_id) REFERENCES chunk_parents(id) ON DELETE CASCADE,UNIQUE(document_id,chunk_index))`,
		`CREATE TABLE IF NOT EXISTS document_links (source_document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,target_document_id INTEGER REFERENCES documents(id) ON DELETE CASCADE,target_raw TEXT NOT NULL,section TEXT NOT NULL DEFAULT '',embedded INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY,name TEXT NOT NULL COLLATE NOCASE UNIQUE,verification_enabled INTEGER NOT NULL DEFAULT 1,project_memory_enabled INTEGER NOT NULL DEFAULT 1,verifier_context_tokens INTEGER NOT NULL DEFAULT 8192,writer_context_tokens INTEGER NOT NULL DEFAULT 16384,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS conversations (id TEXT PRIMARY KEY,project_id TEXT,title TEXT NOT NULL,memory_enabled INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE SET NULL)`,
		`CREATE TABLE IF NOT EXISTS conversation_turns (id INTEGER PRIMARY KEY AUTOINCREMENT,conversation_id TEXT NOT NULL,question TEXT NOT NULL,answer TEXT NOT NULL,sources_json TEXT NOT NULL,chat_model TEXT NOT NULL,agent_metrics_json TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL,FOREIGN KEY(conversation_id) REFERENCES conversations(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS conversation_memory (conversation_id TEXT PRIMARY KEY,summary TEXT NOT NULL DEFAULT '',summarized_until_turn_id INTEGER NOT NULL DEFAULT 0,summarized_turn_count INTEGER NOT NULL DEFAULT 0,summary_model TEXT NOT NULL DEFAULT '',last_error TEXT,updated_at TEXT NOT NULL,FOREIGN KEY(conversation_id) REFERENCES conversations(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS project_memory (project_id TEXT PRIMARY KEY,summary TEXT NOT NULL DEFAULT '',summarized_until_turn_id INTEGER NOT NULL DEFAULT 0,summarized_turn_count INTEGER NOT NULL DEFAULT 0,summary_model TEXT NOT NULL DEFAULT '',last_error TEXT,updated_at TEXT NOT NULL,FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS staged_documents (run_id TEXT NOT NULL,path TEXT NOT NULL,title TEXT NOT NULL,sha256 TEXT NOT NULL,mtime REAL NOT NULL,metadata_json TEXT NOT NULL,links_json TEXT NOT NULL,indexed_at TEXT NOT NULL,PRIMARY KEY(run_id,path))`,
		`CREATE TABLE IF NOT EXISTS staged_chunk_parents (run_id TEXT NOT NULL,document_path TEXT NOT NULL,parent_index INTEGER NOT NULL,heading TEXT NOT NULL,content TEXT NOT NULL,FOREIGN KEY(run_id,document_path) REFERENCES staged_documents(run_id,path) ON DELETE CASCADE,PRIMARY KEY(run_id,document_path,parent_index))`,
		`CREATE TABLE IF NOT EXISTS staged_chunks (run_id TEXT NOT NULL,document_path TEXT NOT NULL,chunk_index INTEGER NOT NULL,heading TEXT NOT NULL,content TEXT NOT NULL,parent_index INTEGER,embedding BLOB NOT NULL,embedding_dim INTEGER NOT NULL,FOREIGN KEY(run_id,document_path) REFERENCES staged_documents(run_id,path) ON DELETE CASCADE,PRIMARY KEY(run_id,document_path,chunk_index))`,
		`CREATE TABLE IF NOT EXISTS staged_document_links (run_id TEXT NOT NULL,source_document_path TEXT NOT NULL,target_document_path TEXT,target_raw TEXT NOT NULL,section TEXT NOT NULL DEFAULT '',embedded INTEGER NOT NULL DEFAULT 0,FOREIGN KEY(run_id,source_document_path) REFERENCES staged_documents(run_id,path) ON DELETE CASCADE)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_document ON chunks(document_id)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_title ON documents(title)`,
		`CREATE INDEX IF NOT EXISTS idx_links_source ON document_links(source_document_id)`,
		`CREATE INDEX IF NOT EXISTS idx_links_target ON document_links(target_document_id)`,
		`CREATE INDEX IF NOT EXISTS idx_staged_chunks_run ON staged_chunks(run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_staged_links_run ON staged_document_links(run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_staged_parents_run ON staged_chunk_parents(run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_conversations_updated ON conversations(updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_conversations_project_updated ON conversations(project_id,updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_conversation_turns_conversation ON conversation_turns(conversation_id,id)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(title,heading,content,path,document_status UNINDEXED,tokenize = 'unicode61 remove_diacritics 2')`,
	}
}

func (s *Store) applyMigrations(ctx context.Context, current int) error {
	for target := current + 1; target <= LatestSchemaVersion; target++ {
		tx, err := s.WriteDB().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		statements := []string{}
		switch target {
		case 1:
			statements = schemaV1Statements()
		default:
			_ = tx.Rollback()
			return fmt.Errorf("migración SQLite %d no implementada", target)
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("no se pudo aplicar la migración SQLite %d: %w", target, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, target)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("no se pudo registrar la migración SQLite %d: %w", target, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("no se pudo confirmar la migración SQLite %d: %w", target, err)
		}
	}
	return nil
}

func (s *Store) ensureDerivedState(ctx context.Context) error {
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := utcNow()
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,created_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, DefaultProjectID, DefaultProjectName, now, now); err != nil {
		return err
	}
	ftsRebuild, err := derivedIndexNeedsRebuild(ctx, tx, FTSVersionMetaKey, FTSIndexVersion, "chunks_fts", "chunks")
	if err != nil {
		return err
	}
	if ftsRebuild {
		if err := rebuildFTS(ctx, tx); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, FTSVersionMetaKey, FTSIndexVersion); err != nil {
			return err
		}
	}
	graphVersion, err := metaValue(ctx, tx, GraphVersionMetaKey)
	if err != nil {
		return err
	}
	if graphVersion != GraphIndexVersion {
		if err := rebuildLinks(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func databaseSchemaVersion(ctx context.Context, database *sql.DB) (int, error) {
	var version int
	err := database.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	return version, err
}

func databaseHasUserSchema(ctx context.Context, database *sql.DB) (bool, error) {
	var exists int
	err := database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name NOT LIKE 'sqlite_%')`).Scan(&exists)
	return exists != 0, err
}

func derivedIndexNeedsRebuild(ctx context.Context, tx *sql.Tx, metaKey, expectedVersion, derivedTable, sourceTable string) (bool, error) {
	version, err := metaValue(ctx, tx, metaKey)
	if err != nil {
		return false, err
	}
	if version != expectedVersion {
		return true, nil
	}
	var derivedCount, sourceCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+derivedTable).Scan(&derivedCount); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+sourceTable).Scan(&sourceCount); err != nil {
		return false, err
	}
	return derivedCount != sourceCount, nil
}

func metaValue(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) backupBeforeMigration(ctx context.Context, from, to int) (string, error) {
	backup := migrationBackupPath(s.path, from, to, time.Now().UTC())
	if _, err := s.WriteDB().ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		_ = os.Remove(backup)
		return "", err
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func migrationBackupPath(path string, from, to int, at time.Time) string {
	extension := filepath.Ext(path)
	if extension == "" {
		extension = ".sqlite3"
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	stamp := at.Format("20060102T150405.000000000Z")
	return fmt.Sprintf("%s.backup-v%d-to-v%d-%s%s", base, from, to, stamp, extension)
}

func (s *Store) LastMigrationBackup() string { return s.lastMigrationBackup }

func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.ReadDB().QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var result Stats
	queries := []struct {
		query  string
		target *int
	}{{`SELECT COUNT(*) FROM documents`, &result.Documents}, {`SELECT COUNT(*) FROM chunks`, &result.Chunks}, {`SELECT COUNT(*) FROM chunk_parents`, &result.Parents}, {`SELECT COUNT(*) FROM document_links`, &result.Links}}
	for _, item := range queries {
		if err := s.ReadDB().QueryRowContext(ctx, item.query).Scan(item.target); err != nil {
			return Stats{}, err
		}
	}
	return result, nil
}

func (s *Store) DocumentHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT path,sha256 FROM documents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var path, sha string
		if err := rows.Scan(&path, &sha); err != nil {
			return nil, err
		}
		result[path] = sha
	}
	return result, rows.Err()
}

func (s *Store) EmbeddingDimensions(ctx context.Context) ([]int, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT DISTINCT embedding_dim FROM chunks ORDER BY embedding_dim`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []int
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) ResetStaged(ctx context.Context) error {
	_, err := s.WriteDB().ExecContext(ctx, `DELETE FROM staged_document_links;DELETE FROM staged_chunks;DELETE FROM staged_chunk_parents;DELETE FROM staged_documents`)
	return err
}
func (s *Store) DiscardStaged(ctx context.Context, runID string) error {
	_, err := s.WriteDB().ExecContext(ctx, `DELETE FROM staged_document_links WHERE run_id=?;DELETE FROM staged_chunks WHERE run_id=?;DELETE FROM staged_chunk_parents WHERE run_id=?;DELETE FROM staged_documents WHERE run_id=?`, runID, runID, runID, runID)
	return err
}

func (s *Store) StageDocument(ctx context.Context, runID string, document Document) error {
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	metadata, links, err := encodeDocument(document)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM staged_documents WHERE run_id=? AND path=?`, runID, document.Path); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO staged_documents(run_id,path,title,sha256,mtime,metadata_json,links_json,indexed_at) VALUES(?,?,?,?,?,?,?,?)`, runID, document.Path, document.Title, document.SHA256, document.MTime, metadata, links, utcNow()); err != nil {
		return err
	}
	for _, parent := range document.Parents {
		if _, err = tx.ExecContext(ctx, `INSERT INTO staged_chunk_parents(run_id,document_path,parent_index,heading,content) VALUES(?,?,?,?,?)`, runID, document.Path, parent.Index, parent.Heading, parent.Content); err != nil {
			return err
		}
	}
	for _, chunk := range document.Chunks {
		if _, err = tx.ExecContext(ctx, `INSERT INTO staged_chunks(run_id,document_path,chunk_index,heading,content,parent_index,embedding,embedding_dim) VALUES(?,?,?,?,?,?,?,?)`, runID, document.Path, chunk.Index, chunk.Heading, chunk.Content, nullableInt(chunk.ParentIndex), encodeFloat32(chunk.Embedding), len(chunk.Embedding)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ResolveStagedLinks(ctx context.Context, runID string) (int, error) {
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT path,title,links_json FROM staged_documents WHERE run_id=? ORDER BY path`, runID)
	if err != nil {
		return 0, err
	}
	var documents []stagedDocument
	for rows.Next() {
		var item stagedDocument
		var raw string
		if err := rows.Scan(&item.path, &item.title, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(raw), &item.links); err != nil {
			rows.Close()
			return 0, err
		}
		documents = append(documents, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	aliases := makeAliasIndex(documents)
	if _, err := tx.ExecContext(ctx, `DELETE FROM staged_document_links WHERE run_id=?`, runID); err != nil {
		return 0, err
	}
	count := 0
	for _, document := range documents {
		for _, link := range document.links {
			target := resolveTarget(aliases, link.Target)
			if _, err := tx.ExecContext(ctx, `INSERT INTO staged_document_links(run_id,source_document_path,target_document_path,target_raw,section,embedded) VALUES(?,?,?,?,?,?)`, runID, document.path, nullableString(target), link.Target, link.Section, boolInt(link.Embedded)); err != nil {
				return 0, err
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) CommitStagedIndex(ctx context.Context, runID, fingerprintKey, fingerprint string, expected Stats) error {
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actual, orphans, err := stagedCounts(ctx, tx, runID)
	if err != nil {
		return err
	}
	if actual.Documents != expected.Documents || actual.Chunks != expected.Chunks || actual.Parents != expected.Parents || actual.Links != expected.Links || orphans != 0 {
		return errors.New("la preparación del índice está incompleta; se conserva el índice anterior")
	}
	commands := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM chunks_fts`, nil}, {`DELETE FROM documents`, nil},
		{`INSERT INTO documents(path,title,sha256,mtime,metadata_json,links_json,indexed_at) SELECT path,title,sha256,mtime,metadata_json,links_json,indexed_at FROM staged_documents WHERE run_id=? ORDER BY path`, []any{runID}},
		{`INSERT INTO chunk_parents(document_id,parent_index,heading,content) SELECT d.id,p.parent_index,p.heading,p.content FROM staged_chunk_parents p JOIN documents d ON d.path=p.document_path WHERE p.run_id=? ORDER BY p.document_path,p.parent_index`, []any{runID}},
		{`INSERT INTO chunks(document_id,chunk_index,heading,content,parent_id,embedding,embedding_dim) SELECT d.id,c.chunk_index,c.heading,c.content,p.id,c.embedding,c.embedding_dim FROM staged_chunks c JOIN documents d ON d.path=c.document_path LEFT JOIN chunk_parents p ON p.document_id=d.id AND p.parent_index=c.parent_index WHERE c.run_id=? ORDER BY c.document_path,c.chunk_index`, []any{runID}},
		{`INSERT INTO document_links(source_document_id,target_document_id,target_raw,section,embedded) SELECT source.id,target.id,l.target_raw,l.section,l.embedded FROM staged_document_links l JOIN documents source ON source.path=l.source_document_path LEFT JOIN documents target ON target.path=l.target_document_path WHERE l.run_id=? ORDER BY l.rowid`, []any{runID}},
	}
	for _, command := range commands {
		if _, err := tx.ExecContext(ctx, command.query, command.args...); err != nil {
			return err
		}
	}
	if err := rebuildFTS(ctx, tx); err != nil {
		return err
	}
	active, err := activeCounts(ctx, tx)
	if err != nil {
		return err
	}
	if active != expected {
		return errors.New("la activación del índice no produjo todos los documentos y fragmentos esperados; se revierte la transacción")
	}
	if err := setMeta(ctx, tx, fingerprintKey, fingerprint); err != nil {
		return err
	}
	if err := setMeta(ctx, tx, GraphVersionMetaKey, GraphIndexVersion); err != nil {
		return err
	}
	for _, table := range []string{"staged_document_links", "staged_chunks", "staged_chunk_parents", "staged_documents"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE run_id=?`, runID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// La activación sustituye todo el corpus; el índice se reconstruirá una sola
	// vez cuando llegue la siguiente actualización incremental.
	s.graphIndex = nil
	return nil
}

func (s *Store) ReplaceDocument(ctx context.Context, document Document) error {
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	index, err := s.ensureActiveGraphIndexLocked(ctx, tx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// El índice se modifica bajo el mismo bloqueo que la transacción. Si
			// SQLite revierte, descartarlo evita publicar un estado especulativo.
			s.graphIndex = nil
		}
	}()
	affectedAliases := map[string]bool{}
	if old, exists := index.documents[document.Path]; exists {
		for _, alias := range documentAliases(old.path, old.title) {
			affectedAliases[alias] = true
		}
	}
	for _, alias := range documentAliases(document.Path, document.Title) {
		affectedAliases[alias] = true
	}
	affectedSources := index.sourcesForAliases(affectedAliases)
	affectedSources[document.Path] = true
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE rowid IN (SELECT c.id FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.path=?)`, document.Path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE path=?`, document.Path); err != nil {
		return err
	}
	metadata, links, err := encodeDocument(document)
	if err != nil {
		return err
	}
	indexedAt := utcNow()
	result, err := tx.ExecContext(ctx, `INSERT INTO documents(path,title,sha256,mtime,metadata_json,links_json,indexed_at) VALUES(?,?,?,?,?,?,?)`, document.Path, document.Title, document.SHA256, document.MTime, metadata, links, indexedAt)
	if err != nil {
		return err
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	parentIDs := map[int]int64{}
	for _, parent := range document.Parents {
		result, err := tx.ExecContext(ctx, `INSERT INTO chunk_parents(document_id,parent_index,heading,content) VALUES(?,?,?,?)`, documentID, parent.Index, parent.Heading, parent.Content)
		if err != nil {
			return err
		}
		parentIDs[parent.Index], err = result.LastInsertId()
		if err != nil {
			return err
		}
	}
	for _, chunk := range document.Chunks {
		var parent any
		if chunk.ParentIndex != nil {
			parent = parentIDs[*chunk.ParentIndex]
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks(document_id,chunk_index,heading,content,parent_id,embedding,embedding_dim) VALUES(?,?,?,?,?,?,?)`, documentID, chunk.Index, chunk.Heading, chunk.Content, parent, encodeFloat32(chunk.Embedding), len(chunk.Embedding)); err != nil {
			return err
		}
	}
	if err := insertDocumentFTS(ctx, tx, documentID); err != nil {
		return err
	}
	index.replace(activeLinkDocument{id: documentID, path: document.Path, title: document.Title, links: document.Links})
	if err := rebuildLinksForPaths(ctx, tx, affectedSources, index); err != nil {
		return err
	}
	signature, err := readActiveGraphSignature(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	index.signature = signature
	committed = true
	return nil
}

func (s *Store) DeleteDocumentsNotIn(ctx context.Context, paths map[string]bool) (int, error) {
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	index, err := s.ensureActiveGraphIndexLocked(ctx, tx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			s.graphIndex = nil
		}
	}()
	var remove []string
	affectedAliases := map[string]bool{}
	for path, document := range index.documents {
		if !paths[path] {
			remove = append(remove, path)
			for _, alias := range documentAliases(path, document.title) {
				affectedAliases[alias] = true
			}
		}
	}
	sort.Strings(remove)
	affectedSources := index.sourcesForAliases(affectedAliases)
	for _, path := range remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE rowid IN (SELECT c.id FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.path=?)`, path); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE path=?`, path); err != nil {
			return 0, err
		}
		index.remove(path)
		delete(affectedSources, path)
	}
	if len(remove) > 0 {
		if err := rebuildLinksForPaths(ctx, tx, affectedSources, index); err != nil {
			return 0, err
		}
	}
	signature, err := readActiveGraphSignature(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	index.signature = signature
	committed = true
	return len(remove), nil
}

// DeleteDocuments borra exactamente los paths dados, a diferencia de
// DeleteDocumentsNotIn que borra el complemento de un conjunto. Sirve a la
// reindexación dirigida, que ya conoce qué rutas se borraron sin necesidad de
// enumerar el vault completo.
func (s *Store) DeleteDocuments(ctx context.Context, paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	index, err := s.ensureActiveGraphIndexLocked(ctx, tx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			s.graphIndex = nil
		}
	}()
	var remove []string
	affectedAliases := map[string]bool{}
	for _, path := range paths {
		document, exists := index.documents[path]
		if !exists {
			continue
		}
		remove = append(remove, path)
		for _, alias := range documentAliases(path, document.title) {
			affectedAliases[alias] = true
		}
	}
	sort.Strings(remove)
	affectedSources := index.sourcesForAliases(affectedAliases)
	for _, path := range remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE rowid IN (SELECT c.id FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.path=?)`, path); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE path=?`, path); err != nil {
			return 0, err
		}
		index.remove(path)
		delete(affectedSources, path)
	}
	if len(remove) > 0 {
		if err := rebuildLinksForPaths(ctx, tx, affectedSources, index); err != nil {
			return 0, err
		}
	}
	signature, err := readActiveGraphSignature(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	index.signature = signature
	committed = true
	return len(remove), nil
}

func activeCounts(ctx context.Context, tx *sql.Tx) (Stats, error) {
	var s Stats
	for _, item := range []struct {
		q string
		p *int
	}{{`SELECT COUNT(*) FROM documents`, &s.Documents}, {`SELECT COUNT(*) FROM chunks`, &s.Chunks}, {`SELECT COUNT(*) FROM chunk_parents`, &s.Parents}, {`SELECT COUNT(*) FROM document_links`, &s.Links}} {
		if err := tx.QueryRowContext(ctx, item.q).Scan(item.p); err != nil {
			return Stats{}, err
		}
	}
	var fts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks_fts`).Scan(&fts); err != nil {
		return Stats{}, err
	}
	if fts != s.Chunks {
		return Stats{}, errors.New("FTS5 no quedó sincronizado")
	}
	return s, nil
}
func stagedCounts(ctx context.Context, tx *sql.Tx, runID string) (Stats, int, error) {
	var s Stats
	for _, item := range []struct {
		q string
		p *int
	}{{`SELECT COUNT(*) FROM staged_documents WHERE run_id=?`, &s.Documents}, {`SELECT COUNT(*) FROM staged_chunks WHERE run_id=?`, &s.Chunks}, {`SELECT COUNT(*) FROM staged_chunk_parents WHERE run_id=?`, &s.Parents}, {`SELECT COUNT(*) FROM staged_document_links WHERE run_id=?`, &s.Links}} {
		if err := tx.QueryRowContext(ctx, item.q, runID).Scan(item.p); err != nil {
			return Stats{}, 0, err
		}
	}
	var orphans int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_chunks c LEFT JOIN staged_chunk_parents p ON p.run_id=c.run_id AND p.document_path=c.document_path AND p.parent_index=c.parent_index WHERE c.run_id=? AND c.parent_index IS NOT NULL AND p.parent_index IS NULL`, runID).Scan(&orphans)
	return s, orphans, err
}

func rebuildFTS(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO chunks_fts(rowid,title,heading,content,path,document_status) SELECT c.id,d.title,c.heading,c.content,d.path,lower(COALESCE(json_extract(d.metadata_json,'$.estado'),json_extract(d.metadata_json,'$.status'),'')) FROM chunks c JOIN documents d ON d.id=c.document_id ORDER BY c.id`)
	return err
}

func insertDocumentFTS(ctx context.Context, tx *sql.Tx, documentID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO chunks_fts(rowid,title,heading,content,path,document_status) SELECT c.id,d.title,c.heading,c.content,d.path,lower(COALESCE(json_extract(d.metadata_json,'$.estado'),json_extract(d.metadata_json,'$.status'),'')) FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.id=? ORDER BY c.id`, documentID)
	return err
}

type activeLinkDocument struct {
	id          int64
	path, title string
	links       []Link
}

type activeGraphSignature struct {
	documents int64
	maxID     int64
}

type aliasOwner struct {
	path string
	id   int64
}

// activeGraphIndex mantiene tanto los propietarios de cada alias como el
// índice inverso de enlaces. Así una sustitución sólo toca el documento y las
// fuentes cuyos destinos pueden cambiar, sin volver a decodificar el corpus.
type activeGraphIndex struct {
	signature      activeGraphSignature
	documents      map[string]activeLinkDocument
	owners         map[string][]aliasOwner
	sourcesByAlias map[string]map[string]struct{}
}

func readActiveGraphSignature(ctx context.Context, tx *sql.Tx) (activeGraphSignature, error) {
	var signature activeGraphSignature
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM documents`).Scan(&signature.documents, &signature.maxID)
	return signature, err
}

func (s *Store) ensureActiveGraphIndexLocked(ctx context.Context, tx *sql.Tx) (*activeGraphIndex, error) {
	signature, err := readActiveGraphSignature(ctx, tx)
	if err != nil {
		return nil, err
	}
	if s.graphIndex != nil && s.graphIndex.signature == signature {
		return s.graphIndex, nil
	}
	index, err := loadActiveGraphIndex(ctx, tx, signature)
	if err != nil {
		return nil, err
	}
	s.graphIndex = index
	s.graphIndexBuilds++
	return index, nil
}

func loadActiveGraphIndex(ctx context.Context, tx *sql.Tx, signature activeGraphSignature) (*activeGraphIndex, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,path,title,links_json FROM documents ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := &activeGraphIndex{
		signature:      signature,
		documents:      map[string]activeLinkDocument{},
		owners:         map[string][]aliasOwner{},
		sourcesByAlias: map[string]map[string]struct{}{},
	}
	for rows.Next() {
		var document activeLinkDocument
		var raw string
		if err := rows.Scan(&document.id, &document.path, &document.title, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &document.links); err != nil {
			return nil, err
		}
		index.add(document)
	}
	return index, rows.Err()
}

func (index *activeGraphIndex) sourcesForAliases(aliases map[string]bool) map[string]bool {
	result := map[string]bool{}
	for alias := range aliases {
		for source := range index.sourcesByAlias[alias] {
			result[source] = true
		}
	}
	return result
}

func (index *activeGraphIndex) replace(document activeLinkDocument) {
	index.remove(document.path)
	index.add(document)
}

func (index *activeGraphIndex) add(document activeLinkDocument) {
	index.documents[document.path] = document
	for _, alias := range documentAliases(document.path, document.title) {
		owners := append(index.owners[alias], aliasOwner{path: document.path, id: document.id})
		sort.Slice(owners, func(i, j int) bool { return owners[i].path < owners[j].path })
		index.owners[alias] = owners
	}
	for _, link := range document.links {
		for _, alias := range targetAliases(link.Target) {
			if index.sourcesByAlias[alias] == nil {
				index.sourcesByAlias[alias] = map[string]struct{}{}
			}
			index.sourcesByAlias[alias][document.path] = struct{}{}
		}
	}
}

func (index *activeGraphIndex) remove(path string) {
	document, exists := index.documents[path]
	if !exists {
		return
	}
	delete(index.documents, path)
	for _, alias := range documentAliases(document.path, document.title) {
		owners := index.owners[alias]
		kept := owners[:0]
		for _, owner := range owners {
			if owner.path != path {
				kept = append(kept, owner)
			}
		}
		if len(kept) == 0 {
			delete(index.owners, alias)
		} else {
			index.owners[alias] = kept
		}
	}
	for _, link := range document.links {
		for _, alias := range targetAliases(link.Target) {
			delete(index.sourcesByAlias[alias], path)
			if len(index.sourcesByAlias[alias]) == 0 {
				delete(index.sourcesByAlias, alias)
			}
		}
	}
}

func (index *activeGraphIndex) resolve(target string) any {
	for _, alias := range targetAliases(target) {
		if owners := index.owners[alias]; len(owners) > 0 {
			return owners[0].id
		}
	}
	return nil
}

func rebuildLinksForPaths(ctx context.Context, tx *sql.Tx, paths map[string]bool, index *activeGraphIndex) error {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		document, exists := index.documents[path]
		if !exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_links WHERE source_document_id=?`, document.id); err != nil {
			return err
		}
		for _, link := range document.links {
			target := index.resolve(link.Target)
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_links(source_document_id,target_document_id,target_raw,section,embedded) VALUES(?,?,?,?,?)`, document.id, target, link.Target, link.Section, boolInt(link.Embedded)); err != nil {
				return err
			}
		}
	}
	return setMeta(ctx, tx, GraphVersionMetaKey, GraphIndexVersion)
}

func rebuildLinks(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,path,title,links_json FROM documents ORDER BY path`)
	if err != nil {
		return err
	}
	type active struct {
		id          int64
		path, title string
		links       []Link
	}
	var docs []active
	for rows.Next() {
		var d active
		var raw string
		if err := rows.Scan(&d.id, &d.path, &d.title, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &d.links); err != nil {
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	aliases := map[string]int64{}
	for _, d := range docs {
		for _, alias := range documentAliases(d.path, d.title) {
			if _, ok := aliases[alias]; !ok {
				aliases[alias] = d.id
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_links`); err != nil {
		return err
	}
	for _, d := range docs {
		for _, link := range d.links {
			var target any
			for _, candidate := range targetAliases(link.Target) {
				if id, ok := aliases[candidate]; ok {
					target = id
					break
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_links(source_document_id,target_document_id,target_raw,section,embedded) VALUES(?,?,?,?,?)`, d.id, target, link.Target, link.Section, boolInt(link.Embedded)); err != nil {
				return err
			}
		}
	}
	return setMeta(ctx, tx, GraphVersionMetaKey, GraphIndexVersion)
}

func encodeDocument(document Document) (string, string, error) {
	metadata, err := json.Marshal(document.Metadata)
	if err != nil {
		return "", "", err
	}
	links, err := json.Marshal(document.Links)
	return string(metadata), string(links), err
}
func encodeFloat32(values []float32) []byte {
	data := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(value))
	}
	return data
}
func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO app_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
func utcNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func normalizeAlias(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
}
func documentAliases(path, title string) []string {
	path = normalizeAlias(path)
	without := strings.TrimSuffix(path, ".md")
	stem := without
	if index := strings.LastIndex(stem, "/"); index >= 0 {
		stem = stem[index+1:]
	}
	values := []string{normalizeAlias(title), without, stem}
	sort.Strings(values)
	return uniqueStrings(values)
}
func targetAliases(target string) []string {
	value := normalizeAlias(target)
	without := strings.TrimSuffix(value, ".md")
	stem := without
	if index := strings.LastIndex(stem, "/"); index >= 0 {
		stem = stem[index+1:]
	}
	return uniqueStrings([]string{value, without, stem})
}
func uniqueStrings(values []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

// stagedDocument evita exportar detalles internos y mantiene determinista la
// regla de Obsidian: la primera ruta ordenada gana una colisión de alias.
type stagedDocument struct {
	path, title string
	links       []Link
}

func makeAliasIndex(documents []stagedDocument) map[string]string {
	result := map[string]string{}
	for _, d := range documents {
		for _, alias := range documentAliases(d.path, d.title) {
			if _, ok := result[alias]; !ok {
				result[alias] = d.path
			}
		}
	}
	return result
}
func resolveTarget(aliases map[string]string, target string) string {
	for _, candidate := range targetAliases(target) {
		if value, ok := aliases[candidate]; ok {
			return value
		}
	}
	return ""
}
