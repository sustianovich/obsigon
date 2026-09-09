package rag

import (
	"context"
	"sort"
	"strings"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

type GraphCandidate struct {
	ChunkID, DocumentID  int64
	Row                  int
	GraphScore           float64
	Hop                  int
	GraphChunkSimilarity float64
}

func GuaranteeGraphNeighbors(selected []map[string]any, graph []GraphCandidate, hydrated map[int64]HydratedChunk, limit int) []map[string]any {
	if len(selected) == 0 || limit <= 0 {
		return nil
	}
	selectedChunks, selectedDocuments := map[int64]bool{}, map[int64]bool{}
	for _, item := range selected {
		selectedChunks[mustInt64(item["chunk_id"])] = true
		selectedDocuments[mustInt64(item["document_id"])] = true
	}
	result := []map[string]any{}
	for _, candidate := range graph {
		if candidate.Hop != 1 || selectedChunks[candidate.ChunkID] || selectedDocuments[candidate.DocumentID] {
			continue
		}
		chunk, ok := hydrated[candidate.ChunkID]
		if !ok {
			continue
		}
		item := chunkMap(chunk)
		score := max(0, candidate.GraphChunkSimilarity*.85)
		item["score"], item["fusion_score"] = score, score
		item["semantic_score"], item["lexical_score"] = nil, nil
		item["graph_score"], item["graph_hop"], item["graph_chunk_similarity"] = candidate.GraphScore, 1, candidate.GraphChunkSimilarity
		item["reason"] = "búsqueda por grafo (enlace directo)"
		item["matched_chunk_ids"] = []int64{candidate.ChunkID}
		result = append(result, item)
		selectedChunks[candidate.ChunkID], selectedDocuments[candidate.DocumentID] = true, true
		if len(result) >= limit {
			break
		}
	}
	return result
}

func (r *Retriever) ExpandWikiLinks(ctx context.Context, selected []map[string]any, status, vigencia string, tags []string, frontmatter map[string]any, limit int) ([]map[string]any, error) {
	if len(selected) == 0 || limit <= 0 {
		return nil, nil
	}
	sourceIDs := []int64{}
	selectedChunks, selectedDocuments := map[int64]bool{}, map[int64]bool{}
	for index, item := range selected {
		selectedChunks[mustInt64(item["chunk_id"])] = true
		selectedDocuments[mustInt64(item["document_id"])] = true
		if index < 3 {
			sourceIDs = append(sourceIDs, mustInt64(item["document_id"]))
		}
	}
	links, err := r.Store.LinkedDocuments(ctx, sourceIDs)
	if err != nil {
		return nil, err
	}
	documentIDs := []int64{}
	for _, link := range links {
		if !selectedDocuments[link.TargetDocumentID] {
			documentIDs = append(documentIDs, link.TargetDocumentID)
		}
	}
	chunkIDs, err := r.Store.DocumentChunkIDs(ctx, documentIDs)
	if err != nil {
		return nil, err
	}
	hydrated, err := r.Vectors.Hydrate(ctx, chunkIDs)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, link := range links {
		if selectedDocuments[link.TargetDocumentID] {
			continue
		}
		var chosen *HydratedChunk
		section := strings.TrimSpace(strings.ToLower(strings.ReplaceAll(link.Section, "#", "")))
		for _, chunk := range hydrated {
			if chunk.DocumentID != link.TargetDocumentID || !passesFilters(chunk, status, vigencia, tags, frontmatter) {
				continue
			}
			copy := chunk
			if chosen == nil || section != "" && strings.Contains(strings.ToLower(chunk.Heading), section) || section == "" && chunk.ChunkIndex < chosen.ChunkIndex {
				chosen = &copy
			}
		}
		if chosen == nil || selectedChunks[chosen.ChunkID] {
			continue
		}
		item := chunkMap(*chosen)
		sourceTitle, sourceScore, sourceFusion := "documento", 0.0, 0.0
		for _, source := range selected {
			if mustInt64(source["document_id"]) == link.SourceDocumentID {
				sourceTitle = stringValue(source["title"])
				sourceScore, sourceFusion = floatValue(source["score"]), floatValue(source["fusion_score"])
				break
			}
		}
		item["score"], item["fusion_score"] = max(0, sourceScore*.85), max(0, sourceFusion*.85)
		item["semantic_score"], item["lexical_score"] = nil, nil
		item["reason"] = "enlace desde «" + sourceTitle + "»"
		item["matched_chunk_ids"] = []int64{chosen.ChunkID}
		result = append(result, item)
		selectedChunks[chosen.ChunkID], selectedDocuments[chosen.DocumentID] = true, true
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func PropagateDocumentScores(seeds map[int64]float64, edges []db.GraphEdge, maxHops int, decay, backlinkWeight float64) (map[int64]float64, map[int64]int) {
	type neighbor struct {
		id     int64
		weight float64
	}
	adjacency := map[int64][]neighbor{}
	for _, edge := range edges {
		adjacency[edge.SourceDocumentID] = append(adjacency[edge.SourceDocumentID], neighbor{edge.TargetDocumentID, 1})
		adjacency[edge.TargetDocumentID] = append(adjacency[edge.TargetDocumentID], neighbor{edge.SourceDocumentID, backlinkWeight})
	}
	bestSeen, frontier := map[int64]float64{}, map[int64]float64{}
	for id, score := range seeds {
		bestSeen[id], frontier[id] = score, score
	}
	reached, hops := map[int64]float64{}, map[int64]int{}
	for hop := 1; hop <= maxHops; hop++ {
		next := map[int64]float64{}
		for id, score := range frontier {
			for _, edge := range adjacency[id] {
				propagated := score * decay * edge.weight
				if previous, ok := bestSeen[edge.id]; ok && propagated <= previous {
					continue
				}
				bestSeen[edge.id] = propagated
				if propagated > next[edge.id] {
					next[edge.id] = propagated
				}
				if _, seed := seeds[edge.id]; !seed {
					reached[edge.id], hops[edge.id] = propagated, hop
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return reached, hops
}

func GraphCandidates(ctx context.Context, semantic []SemanticCandidate, query []float32, settings config.Settings, store *db.Store, vectors *VectorStore) ([]GraphCandidate, error) {
	if !settings.GraphSearchEnabled || len(semantic) == 0 {
		return nil, nil
	}
	seeds := map[int64]float64{}
	for _, item := range semantic {
		if _, exists := seeds[item.DocumentID]; exists {
			continue
		}
		seeds[item.DocumentID] = 1 / float64(len(seeds)+1)
		if len(seeds) >= settings.GraphSeedDocuments {
			break
		}
	}
	edges, err := store.GraphEdges(ctx)
	if err != nil {
		return nil, err
	}
	scores, hops := PropagateDocumentScores(seeds, edges, settings.GraphMaxHops, settings.GraphDecay, settings.GraphBacklinkWeight)
	if len(scores) == 0 {
		return nil, nil
	}
	result, err := vectors.BestChunksForDocuments(ctx, query, scores)
	if err != nil {
		return nil, err
	}
	for index := range result {
		result[index].Hop = hops[result[index].DocumentID]
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].GraphScore == result[j].GraphScore {
			return result[i].GraphChunkSimilarity > result[j].GraphChunkSimilarity
		}
		return result[i].GraphScore > result[j].GraphScore
	})
	if len(result) > settings.GraphMaxCandidates {
		result = result[:settings.GraphMaxCandidates]
	}
	return result, nil
}
