package rag

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

type QueryClass string

const (
	QueryFactual    QueryClass = "factual"
	QueryRelational QueryClass = "relational"
	QueryHybrid     QueryClass = "hybrid"
	QueryUnknown    QueryClass = "unknown"
)

// RetrievalPolicy se crea de nuevo para cada petición. Retrieve sólo usa una
// copia local de Settings, por lo que dos consultas concurrentes nunca mutan
// ni comparten su política efectiva.
type RetrievalPolicy struct {
	Class        QueryClass `json:"class"`
	GraphEnabled bool       `json:"graph_enabled"`
	GraphMaxHops int        `json:"graph_max_hops"`
	GraphWeight  float64    `json:"graph_weight"`
	Reasons      []string   `json:"reasons"`
}

func RouteQuery(ctx context.Context, question string, semantic []SemanticCandidate, settings config.Settings, store *db.Store) (RetrievalPolicy, error) {
	normalized := normalizeLexical(question)
	reasons := []string{}
	relational := false
	for _, raw := range settings.QueryRouterRelationalPatterns {
		pattern := normalizeLexical(strings.TrimSpace(raw))
		if pattern != "" && strings.Contains(normalized, pattern) {
			relational = true
			reasons = append(reasons, "patrón_relacional:"+raw)
			break
		}
	}

	aliases, err := mentionedDocumentAliases(ctx, normalized, store)
	if err != nil {
		return RetrievalPolicy{}, err
	}
	if len(aliases) > 0 {
		reasons = append(reasons, fmt.Sprintf("notas_mencionadas:%d", len(aliases)))
	}
	if len(aliases) >= 2 {
		relational = true
	}

	connected, err := connectedSemanticSeeds(ctx, semantic, settings.GraphSeedDocuments, store)
	if err != nil {
		return RetrievalPolicy{}, err
	}
	if connected > 0 {
		reasons = append(reasons, fmt.Sprintf("semillas_conectadas:%d", connected))
		relational = true
	}

	structuralHits := 0
	for _, raw := range settings.QueryRouterStructuralTerms {
		term := normalizeLexical(strings.TrimSpace(raw))
		if term != "" {
			structuralHits += strings.Count(normalized, term)
		}
	}
	if structuralHits >= settings.QueryRouterStructuralThreshold {
		relational = true
		reasons = append(reasons, fmt.Sprintf("sustantivos_estructurales:%d", structuralHits))
	}

	terms := ExtractLexicalTerms(question, 32)
	factual := containsQuestionCue(normalized) || len(terms) >= 2
	class := QueryUnknown
	switch {
	case relational && factual && (connected > 0 || structuralHits >= settings.QueryRouterStructuralThreshold):
		class = QueryHybrid
	case relational:
		class = QueryRelational
	case factual:
		class = QueryFactual
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "sin_señales_relacionales")
	}
	graphEnabled := class == QueryRelational || class == QueryHybrid
	return RetrievalPolicy{Class: class, GraphEnabled: graphEnabled, GraphMaxHops: settings.GraphMaxHops, GraphWeight: settings.GraphWeight, Reasons: reasons}, nil
}

func mentionedDocumentAliases(ctx context.Context, normalizedQuestion string, store *db.Store) ([]int64, error) {
	rows, err := store.ReadDB().QueryContext(ctx, `SELECT id,path,title FROM documents ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []int64{}
	for rows.Next() {
		var id int64
		var path, title string
		if err := rows.Scan(&id, &path, &title); err != nil {
			return nil, err
		}
		stem := strings.TrimSuffix(filepath.Base(filepath.ToSlash(path)), filepath.Ext(path))
		for _, alias := range []string{title, stem, strings.TrimSuffix(filepath.ToSlash(path), filepath.Ext(path))} {
			normalizedAlias := normalizeLexical(alias)
			if len([]rune(normalizedAlias)) >= 4 && strings.Contains(normalizedQuestion, normalizedAlias) {
				result = append(result, id)
				break
			}
		}
	}
	return result, rows.Err()
}

func connectedSemanticSeeds(ctx context.Context, semantic []SemanticCandidate, limit int, store *db.Store) (int, error) {
	ids := []int64{}
	seen := map[int64]bool{}
	for _, candidate := range semantic {
		if !seen[candidate.DocumentID] {
			seen[candidate.DocumentID] = true
			ids = append(ids, candidate.DocumentID)
			if len(ids) >= max(1, limit) {
				break
			}
		}
	}
	connected := 0
	for _, id := range ids {
		var exists int
		if err := store.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM document_links WHERE target_document_id IS NOT NULL AND (source_document_id=? OR target_document_id=?))`, id, id).Scan(&exists); err != nil {
			return 0, err
		}
		connected += exists
	}
	return connected, nil
}

func containsQuestionCue(value string) bool {
	words := strings.Fields(value)
	sort.Strings(words)
	for _, cue := range []string{"cual", "cuando", "donde", "por que", "que", "quien"} {
		if strings.Contains(value, cue) {
			return true
		}
	}
	return false
}
