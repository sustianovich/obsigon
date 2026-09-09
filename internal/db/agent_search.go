package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// NoteRecord is the document-level view used by the agentic retrieval tools.
// It deliberately excludes embeddings and chunks: search tools return small
// note cards and open/read tools fetch the larger body separately.
type NoteRecord struct {
	ID       int64          `json:"-"`
	Path     string         `json:"path"`
	Title    string         `json:"title"`
	MTime    float64        `json:"mtime"`
	Metadata map[string]any `json:"metadata"`
}

type NoteNeighbor struct {
	NoteRecord
	Direction string `json:"direction"`
	Section   string `json:"section,omitempty"`
}

type NoteMatch struct {
	NoteRecord
	Heading string `json:"heading"`
	Content string `json:"content"`
}

func (s *Store) FindNote(ctx context.Context, pathOrTitle string) (NoteRecord, error) {
	value := strings.TrimSpace(strings.ReplaceAll(pathOrTitle, `\`, "/"))
	if value == "" {
		return NoteRecord{}, ErrNotFound
	}
	row := s.ReadDB().QueryRowContext(ctx, `
		SELECT id,path,title,mtime,metadata_json
		FROM documents
		WHERE lower(path)=lower(?) OR lower(title)=lower(?)
		ORDER BY CASE WHEN lower(path)=lower(?) THEN 0 ELSE 1 END,id
		LIMIT 1`, value, value, value)
	return scanNote(row)
}

func (s *Store) NotesByTag(ctx context.Context, tag string, limit int) ([]NoteRecord, error) {
	wanted := normalizeAgentTag(tag)
	if wanted == "" {
		return nil, nil
	}
	notes, err := s.allNoteRecords(ctx, `ORDER BY lower(title),path`)
	if err != nil {
		return nil, err
	}
	result := make([]NoteRecord, 0, min(limit, len(notes)))
	for _, note := range notes {
		for _, candidate := range metadataStringList(note.Metadata["_tags"]) {
			candidate = normalizeAgentTag(candidate)
			if candidate == wanted || strings.HasPrefix(candidate, wanted+"/") {
				result = append(result, note)
				break
			}
		}
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *Store) NotesByFrontmatter(ctx context.Context, key string, value any, limit int) ([]NoteRecord, error) {
	wantedKey := strings.ToLower(strings.TrimSpace(key))
	if wantedKey == "" || strings.HasPrefix(wantedKey, "_") {
		return nil, nil
	}
	notes, err := s.allNoteRecords(ctx, `ORDER BY lower(title),path`)
	if err != nil {
		return nil, err
	}
	result := make([]NoteRecord, 0, min(limit, len(notes)))
	for _, note := range notes {
		for candidateKey, candidateValue := range note.Metadata {
			if strings.EqualFold(strings.TrimSpace(candidateKey), wantedKey) && metadataValueMatches(candidateValue, value) {
				result = append(result, note)
				break
			}
		}
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *Store) DocumentIDsByFrontmatter(ctx context.Context, filters map[string]any) (map[int64]bool, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	notes, err := s.allNoteRecords(ctx, `ORDER BY id`)
	if err != nil {
		return nil, err
	}
	result := map[int64]bool{}
	for _, note := range notes {
		matches := true
		for wantedKey, wantedValue := range filters {
			if strings.HasPrefix(strings.TrimSpace(wantedKey), "_") {
				matches = false
				break
			}
			found := false
			for candidateKey, candidateValue := range note.Metadata {
				if strings.EqualFold(strings.TrimSpace(candidateKey), strings.TrimSpace(wantedKey)) && metadataValueMatches(candidateValue, wantedValue) {
					found = true
					break
				}
			}
			if !found {
				matches = false
				break
			}
		}
		if matches {
			result[note.ID] = true
		}
	}
	return result, nil
}

func (s *Store) RecentNotes(ctx context.Context, limit int) ([]NoteRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.queryNoteRecords(ctx, `SELECT id,path,title,mtime,metadata_json FROM documents ORDER BY mtime DESC,path LIMIT ?`, limit)
}

func (s *Store) NotesByMTime(ctx context.Context, fromUnix, toUnix float64, limit int) ([]NoteRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	if fromUnix > 0 && toUnix > 0 && fromUnix > toUnix {
		fromUnix, toUnix = toUnix, fromUnix
	}
	query := `SELECT id,path,title,mtime,metadata_json FROM documents WHERE 1=1`
	args := []any{}
	if fromUnix > 0 {
		query += ` AND mtime>=?`
		args = append(args, fromUnix)
	}
	if toUnix > 0 {
		query += ` AND mtime<=?`
		args = append(args, toUnix)
	}
	query += ` ORDER BY mtime DESC,path LIMIT ?`
	args = append(args, limit)
	return s.queryNoteRecords(ctx, query, args...)
}

func (s *Store) NoteNeighbors(ctx context.Context, pathOrTitle string, limit int) ([]NoteNeighbor, error) {
	note, err := s.FindNote(ctx, pathOrTitle)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT n.id,n.path,n.title,n.mtime,n.metadata_json,
		       CASE WHEN l.source_document_id=? THEN 'outgoing' ELSE 'backlink' END,
		       l.section
		FROM document_links l
		JOIN documents n ON n.id=CASE WHEN l.source_document_id=? THEN l.target_document_id ELSE l.source_document_id END
		WHERE l.target_document_id IS NOT NULL AND (l.source_document_id=? OR l.target_document_id=?)
		ORDER BY CASE WHEN l.source_document_id=? THEN 0 ELSE 1 END,lower(n.title),n.path
		LIMIT ?`, note.ID, note.ID, note.ID, note.ID, note.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NoteNeighbor{}
	seen := map[int64]bool{}
	for rows.Next() {
		var item NoteNeighbor
		var raw string
		if err := rows.Scan(&item.ID, &item.Path, &item.Title, &item.MTime, &raw, &item.Direction, &item.Section); err != nil {
			return nil, err
		}
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		item.Metadata = decodeNoteMetadata(raw)
		result = append(result, item)
	}
	return result, rows.Err()
}

// GrepNotes uses Go's RE2-based regexp engine, so model-provided expressions
// cannot trigger catastrophic backtracking. Matching is performed over indexed
// paths, titles, headings and chunk text and returns compact exact evidence.
func (s *Store) GrepNotes(ctx context.Context, expression string, limit int) ([]NoteMatch, error) {
	if len(expression) > 256 {
		return nil, fmt.Errorf("la expresión regular no puede superar 256 caracteres")
	}
	pattern, err := regexp.Compile("(?i)" + expression)
	if err != nil {
		return nil, fmt.Errorf("expresión regular no válida: %w", err)
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT d.id,d.path,d.title,d.mtime,d.metadata_json,c.heading,c.content
		FROM chunks c JOIN documents d ON d.id=c.document_id
		ORDER BY d.path,c.chunk_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NoteMatch{}
	for rows.Next() {
		var item NoteMatch
		var raw string
		if err := rows.Scan(&item.ID, &item.Path, &item.Title, &item.MTime, &raw, &item.Heading, &item.Content); err != nil {
			return nil, err
		}
		haystack := strings.Join([]string{item.Path, item.Title, item.Heading, item.Content}, "\n")
		if !pattern.MatchString(haystack) {
			continue
		}
		item.Metadata = decodeNoteMetadata(raw)
		item.Content = regexpExcerpt(item.Content, pattern, 800)
		result = append(result, item)
		if len(result) >= limit {
			break
		}
	}
	return result, rows.Err()
}

// IndexedNoteSections is a fallback for a missing/moved vault file. Parent
// chunks are preferred because they are non-overlapping and already represent
// the larger context selected at index time.
func (s *Store) IndexedNoteSections(ctx context.Context, documentID int64) ([][2]string, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT heading,content FROM chunk_parents WHERE document_id=? ORDER BY parent_index`, documentID)
	if err != nil {
		return nil, err
	}
	result, err := scanSections(rows)
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		return result, nil
	}
	rows, err = s.ReadDB().QueryContext(ctx, `SELECT heading,content FROM chunks WHERE document_id=? ORDER BY chunk_index`, documentID)
	if err != nil {
		return nil, err
	}
	return scanSections(rows)
}

func scanSections(rows *sql.Rows) ([][2]string, error) {
	defer rows.Close()
	result := [][2]string{}
	for rows.Next() {
		var heading, content string
		if err := rows.Scan(&heading, &content); err != nil {
			return nil, err
		}
		result = append(result, [2]string{heading, content})
	}
	return result, rows.Err()
}

func (s *Store) allNoteRecords(ctx context.Context, order string) ([]NoteRecord, error) {
	return s.queryNoteRecords(ctx, `SELECT id,path,title,mtime,metadata_json FROM documents `+order)
}

func (s *Store) queryNoteRecords(ctx context.Context, query string, args ...any) ([]NoteRecord, error) {
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NoteRecord{}
	for rows.Next() {
		var item NoteRecord
		var raw string
		if err := rows.Scan(&item.ID, &item.Path, &item.Title, &item.MTime, &raw); err != nil {
			return nil, err
		}
		item.Metadata = decodeNoteMetadata(raw)
		result = append(result, item)
	}
	return result, rows.Err()
}

type noteScanner interface{ Scan(...any) error }

func scanNote(row noteScanner) (NoteRecord, error) {
	var item NoteRecord
	var raw string
	if err := row.Scan(&item.ID, &item.Path, &item.Title, &item.MTime, &raw); err != nil {
		if err == sql.ErrNoRows {
			return NoteRecord{}, ErrNotFound
		}
		return NoteRecord{}, err
	}
	item.Metadata = decodeNoteMetadata(raw)
	return item, nil
}

func decodeNoteMetadata(raw string) map[string]any {
	result := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &result)
	return result
}

func normalizeAgentTag(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "#/"))
}

func metadataStringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, fmt.Sprint(item))
		}
		return result
	case []string:
		return typed
	case string:
		return []string{typed}
	default:
		return nil
	}
}

func metadataValueMatches(candidate, wanted any) bool {
	wantedText := strings.ToLower(strings.TrimSpace(fmt.Sprint(wanted)))
	if wantedText == "" {
		return candidate == nil || strings.TrimSpace(fmt.Sprint(candidate)) == ""
	}
	values := metadataStringList(candidate)
	if len(values) == 0 {
		values = []string{fmt.Sprint(candidate)}
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wantedText) {
			return true
		}
	}
	return false
}

func regexpExcerpt(content string, pattern *regexp.Regexp, maxRunes int) string {
	indices := pattern.FindStringIndex(content)
	if indices == nil {
		return content
	}
	runes := []rune(content)
	prefixRunes := len([]rune(content[:indices[0]]))
	start := max(0, prefixRunes-maxRunes/3)
	end := min(len(runes), start+maxRunes)
	excerpt := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if end < len(runes) {
		excerpt += "…"
	}
	return excerpt
}
