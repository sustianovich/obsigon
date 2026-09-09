package db

import (
	"context"
	"sort"
)

type GraphEdge struct {
	SourceDocumentID int64
	TargetDocumentID int64
}

type LinkedDocument struct {
	SourceDocumentID int64
	TargetDocumentID int64
	Section          string
}

type GraphStatus struct {
	Available        bool `json:"available"`
	TotalEdges       int  `json:"total_edges"`
	ResolvedEdges    int  `json:"resolved_edges"`
	BrokenEdges      int  `json:"broken_edges"`
	OrphanDocuments  int  `json:"orphan_documents"`
	AmbiguousAliases int  `json:"ambiguous_aliases"`
}

func (s *Store) GraphEdges(ctx context.Context) ([]GraphEdge, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT source_document_id,target_document_id FROM document_links WHERE target_document_id IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []GraphEdge{}
	for rows.Next() {
		var edge GraphEdge
		if err := rows.Scan(&edge.SourceDocumentID, &edge.TargetDocumentID); err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

func (s *Store) LinkedDocuments(ctx context.Context, sourceIDs []int64) ([]LinkedDocument, error) {
	result := []LinkedDocument{}
	for _, sourceID := range sourceIDs {
		rows, err := s.ReadDB().QueryContext(ctx, `SELECT source_document_id,target_document_id,section FROM document_links WHERE source_document_id=? AND target_document_id IS NOT NULL ORDER BY rowid`, sourceID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item LinkedDocument
			if err := rows.Scan(&item.SourceDocumentID, &item.TargetDocumentID, &item.Section); err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, item)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) GraphStatus(ctx context.Context) (GraphStatus, error) {
	result := GraphStatus{Available: true}
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*),COUNT(target_document_id),COALESCE(SUM(CASE WHEN target_document_id IS NULL THEN 1 ELSE 0 END),0) FROM document_links`).Scan(&result.TotalEdges, &result.ResolvedEdges, &result.BrokenEdges); err != nil {
		return GraphStatus{}, err
	}
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM documents d WHERE NOT EXISTS (SELECT 1 FROM document_links l WHERE l.target_document_id IS NOT NULL AND (l.source_document_id=d.id OR l.target_document_id=d.id))`).Scan(&result.OrphanDocuments); err != nil {
		return GraphStatus{}, err
	}
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT id,path,title FROM documents ORDER BY id`)
	if err != nil {
		return GraphStatus{}, err
	}
	defer rows.Close()
	claimants := map[string]map[int64]bool{}
	for rows.Next() {
		var id int64
		var path, title string
		if err := rows.Scan(&id, &path, &title); err != nil {
			return GraphStatus{}, err
		}
		aliases := documentAliases(path, title)
		sort.Strings(aliases)
		for _, alias := range aliases {
			if claimants[alias] == nil {
				claimants[alias] = map[int64]bool{}
			}
			claimants[alias][id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return GraphStatus{}, err
	}
	for _, ids := range claimants {
		if len(ids) > 1 {
			result.AmbiguousAliases++
		}
	}
	return result, nil
}

func (s *Store) DocumentChunkIDs(ctx context.Context, documentIDs []int64) ([]int64, error) {
	if len(documentIDs) == 0 {
		return nil, nil
	}
	result := []int64{}
	for _, documentID := range documentIDs {
		rows, err := s.ReadDB().QueryContext(ctx, `SELECT id FROM chunks WHERE document_id=? ORDER BY chunk_index`, documentID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, id)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// BacklinkCounts returns distinct inbound-link counts per resolved document.
// MOC/index notes naturally receive a high value without relying on filenames.
func (s *Store) BacklinkCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `
		SELECT target_document_id,COUNT(DISTINCT source_document_id)
		FROM document_links
		WHERE target_document_id IS NOT NULL
		GROUP BY target_document_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]int{}
	for rows.Next() {
		var documentID int64
		var count int
		if err := rows.Scan(&documentID, &count); err != nil {
			return nil, err
		}
		result[documentID] = count
	}
	return result, rows.Err()
}
