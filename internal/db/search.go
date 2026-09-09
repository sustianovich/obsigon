package db

import (
	"context"
	"database/sql"
	"strings"
)

type FTSResult struct {
	ChunkID int64
	BM25    float64
}
type FTSStatus struct {
	Available      bool    `json:"available"`
	Version        string  `json:"version"`
	IndexedChunks  int     `json:"indexed_chunks"`
	ExpectedChunks int     `json:"expected_chunks"`
	Synchronized   bool    `json:"synchronized"`
	Error          *string `json:"error"`
}

func (s *Store) SearchFTS(ctx context.Context, matchQuery string, limit int, status string) ([]FTSResult, error) {
	if strings.TrimSpace(matchQuery) == "" || limit <= 0 {
		return nil, nil
	}
	query := `SELECT rowid,bm25(chunks_fts,5.0,3.0,1.0,2.0,0.0) FROM chunks_fts WHERE chunks_fts MATCH ?`
	args := []any{matchQuery}
	if strings.TrimSpace(status) != "" {
		query += ` AND document_status=?`
		args = append(args, strings.ToLower(strings.TrimSpace(status)))
	}
	query += ` ORDER BY bm25(chunks_fts,5.0,3.0,1.0,2.0,0.0) LIMIT ?`
	args = append(args, limit)
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FTSResult
	for rows.Next() {
		var item FTSResult
		if err := rows.Scan(&item.ChunkID, &item.BM25); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) FTSStatus(ctx context.Context) (FTSStatus, error) {
	result := FTSStatus{}
	var name string
	err := s.ReadDB().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='chunks_fts'`).Scan(&name)
	if err == sql.ErrNoRows {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Available = true
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&result.ExpectedChunks); err != nil {
		return result, err
	}
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks_fts`).Scan(&result.IndexedChunks); err != nil {
		return result, err
	}
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, FTSVersionMetaKey).Scan(&result.Version)
	result.Synchronized = result.Available && result.Version == FTSIndexVersion && result.IndexedChunks == result.ExpectedChunks
	return result, nil
}
