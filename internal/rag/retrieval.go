package rag

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

var spanishStopwords = map[string]bool{"a": true, "al": true, "algo": true, "ante": true, "como": true, "con": true, "cual": true, "cuando": true, "de": true, "del": true, "desde": true, "donde": true, "el": true, "ella": true, "en": true, "entre": true, "es": true, "esta": true, "este": true, "esto": true, "hay": true, "la": true, "las": true, "lo": true, "los": true, "mas": true, "me": true, "para": true, "pero": true, "por": true, "porque": true, "que": true, "se": true, "segun": true, "ser": true, "si": true, "sin": true, "sobre": true, "son": true, "su": true, "sus": true, "un": true, "una": true, "y": true, "ya": true}

type RetrievalRequest struct {
	Question         string
	SemanticQuestion string
	TopK             int
	MinSimilarity    float64
	Status, Vigencia string
	Tags             []string
	Frontmatter      map[string]any
	ExpandLinks      bool
}
type RetrievalResult struct {
	Hits             []map[string]any `json:"hits"`
	Abstained        bool             `json:"abstained"`
	AbstentionReason string           `json:"abstention_reason,omitempty"`
	Rerank           RerankOutcome    `json:"rerank"`
	Routing          *RetrievalPolicy `json:"routing,omitempty"`
	Trace            RetrievalTrace   `json:"trace"`
}

type RetrievalTrace struct {
	Version          string                    `json:"version"`
	SemanticBackend  string                    `json:"semantic_backend"`
	Abstained        bool                      `json:"abstained"`
	AbstentionReason string                    `json:"abstention_reason,omitempty"`
	Routing          *RetrievalPolicy          `json:"routing,omitempty"`
	Counts           RetrievalTraceCounts      `json:"counts"`
	Candidates       []RetrievalTraceCandidate `json:"candidates"`
}

type RetrievalTraceCounts struct {
	Semantic int `json:"semantic"`
	Lexical  int `json:"lexical"`
	Graph    int `json:"graph"`
	Fused    int `json:"fused"`
	Selected int `json:"selected"`
}

type RetrievalTraceCandidate struct {
	ChunkID       int64    `json:"chunk_id"`
	Path          string   `json:"path"`
	Heading       string   `json:"heading"`
	SemanticScore *float64 `json:"semantic_score"`
	LexicalScore  *float64 `json:"lexical_score"`
	GraphScore    *float64 `json:"graph_score"`
	FusionScore   *float64 `json:"fusion_score"`
	MMRScore      *float64 `json:"mmr_score"`
	RerankScore   *float64 `json:"rerank_score"`
	BacklinkCount int      `json:"backlink_count"`
	HubScore      *float64 `json:"hub_score"`
	HubBoost      *float64 `json:"hub_boost"`
	RecencyScore  *float64 `json:"recency_score"`
	RecencyBoost  *float64 `json:"recency_boost"`
	FinalScore    *float64 `json:"final_score"`
}
type Retriever struct {
	Settings config.Settings
	Store    *db.Store
	Vectors  *VectorStore
	Embedder Embedder
	Reranker Reranker
}

func ExtractLexicalTerms(value string, limit int) []string {
	if limit <= 0 {
		limit = 32
	}
	result := []string{}
	seen := map[string]bool{}
	for _, token := range unicodeTokens(value) {
		term := strings.Trim(strings.ToLower(token), "_")
		normalized := normalizeLexical(term)
		if normalized == "" || spanishStopwords[normalized] || (len([]rune(normalized)) < 2 && !allDigits(normalized)) || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, term)
		if len(result) >= limit {
			break
		}
	}
	return result
}
func BuildFTSMatchQuery(value string) (string, []string) {
	terms := ExtractLexicalTerms(value, 32)
	quoted := make([]string, len(terms))
	for index, term := range terms {
		quoted[index] = `"` + term + `"`
	}
	return strings.Join(quoted, " OR "), terms
}
func RequiredLexicalMatches(termCount int) int {
	if termCount <= 1 {
		return 1
	}
	if termCount <= 10 {
		return 2
	}
	return max(2, int(math.Ceil(float64(termCount)*.2)))
}

func LexicalTermCoverage(chunk map[string]any, terms []string) (float64, int) {
	if len(terms) == 0 {
		return 0, 0
	}
	content := stringValue(chunk["matched_content"])
	if content == "" {
		content = stringValue(chunk["content"])
	}
	haystack := normalizeLexical(strings.Join([]string{stringValue(chunk["title"]), stringValue(chunk["heading"]), content, stringValue(chunk["path"])}, " "))
	available := map[string]bool{}
	for _, term := range unicodeTokens(haystack) {
		term = strings.Trim(term, "_")
		if term != "" {
			available[term] = true
		}
	}
	matched := 0
	for _, term := range terms {
		if available[normalizeLexical(term)] {
			matched++
		}
	}
	return float64(matched) / float64(len(terms)), matched
}

func ApplyRelativeCutoff(candidates []SemanticCandidate, ratio float64) []SemanticCandidate {
	if ratio <= 0 || len(candidates) == 0 {
		return candidates
	}
	best := candidates[0].Score
	if best <= 0 {
		return candidates
	}
	floor := best * ratio
	result := make([]SemanticCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score >= floor {
			result = append(result, candidate)
		}
	}
	if len(result) == 0 {
		return candidates[:1]
	}
	return result
}

func (r *Retriever) Retrieve(ctx context.Context, request RetrievalRequest) (RetrievalResult, error) {
	if request.TopK <= 0 {
		request.TopK = r.Settings.TopK
	}
	if request.MinSimilarity == 0 {
		request.MinSimilarity = r.Settings.MinSimilarity
	}
	status, vigencia := strings.ToLower(strings.TrimSpace(request.Status)), strings.ToLower(strings.TrimSpace(request.Vigencia))
	tags := normalizeTagFilters(request.Tags)
	scheme, err := ResolveScheme(r.Settings.EmbeddingModel, r.Settings.EmbeddingPrefixScheme)
	if err != nil {
		return RetrievalResult{}, err
	}
	semanticQuestion := strings.TrimSpace(request.SemanticQuestion)
	if semanticQuestion == "" {
		semanticQuestion = request.Question
	}
	embeddings, err := r.Embedder.Embed(ctx, []string{scheme.Query + semanticQuestion})
	if err != nil {
		return RetrievalResult{}, err
	}
	if len(embeddings) != 1 {
		return RetrievalResult{}, fmt.Errorf("Ollama no devolvió el embedding de la consulta")
	}
	candidateLimit := max(20, request.TopK*r.Settings.HybridCandidateMultiplier)
	allowedDocuments, err := r.Store.DocumentIDsByFrontmatter(ctx, request.Frontmatter)
	if err != nil {
		return RetrievalResult{}, err
	}
	semantic, semanticBackend, err := r.Vectors.SearchDocumentsWithBackend(ctx, embeddings[0], candidateLimit, request.MinSimilarity, status, vigencia, tags, allowedDocuments)
	if err != nil {
		return RetrievalResult{}, err
	}
	semantic = ApplyRelativeCutoff(semantic, r.Settings.MinRelativeScore)
	// Invariante central: texto, grafo y chat no pueden rescatar una consulta
	// que no superó el umbral de coseno.
	if len(semantic) == 0 {
		reranker := r.Reranker
		if reranker == nil {
			reranker = NewNoopReranker()
		}
		availability := reranker.Availability(ctx)
		reason := "ningún fragmento superó RAG_MIN_SIMILARITY"
		return RetrievalResult{Hits: []map[string]any{}, Abstained: true, AbstentionReason: reason, Rerank: RerankOutcome{Status: "not_run", Backend: availability.Backend, Available: availability.Available}, Trace: RetrievalTrace{Version: "1", SemanticBackend: semanticBackend, Abstained: true, AbstentionReason: reason, Counts: RetrievalTraceCounts{}, Candidates: []RetrievalTraceCandidate{}}}, nil
	}
	effective := r.Settings
	var routing *RetrievalPolicy
	if r.Settings.QueryRoutingEnabled {
		policy, routeErr := RouteQuery(ctx, request.Question, semantic, r.Settings, r.Store)
		if routeErr != nil {
			return RetrievalResult{}, routeErr
		}
		routing = &policy
		effective.GraphSearchEnabled = policy.GraphEnabled
		effective.GraphMaxHops = policy.GraphMaxHops
		effective.GraphWeight = policy.GraphWeight
	}

	graph, err := GraphCandidates(ctx, semantic, embeddings[0], effective, r.Store, r.Vectors)
	if err != nil {
		return RetrievalResult{}, err
	}

	lexicalTerms := []string{}
	lexical := []db.FTSResult{}
	if r.Settings.HybridSearchEnabled {
		match, terms := BuildFTSMatchQuery(request.Question)
		lexicalTerms = terms
		if match != "" {
			ftsStatus, statusErr := r.Store.FTSStatus(ctx)
			if statusErr != nil {
				return RetrievalResult{}, statusErr
			}
			if ftsStatus.Synchronized {
				lexical, err = r.Store.SearchFTS(ctx, match, candidateLimit*2, status)
				if err != nil {
					return RetrievalResult{}, err
				}
			}
		}
	}
	ids := make([]int64, 0, len(semantic)+len(lexical))
	for _, candidate := range semantic {
		ids = append(ids, candidate.ChunkID)
	}
	for _, candidate := range lexical {
		ids = append(ids, candidate.ChunkID)
	}
	for _, candidate := range graph {
		ids = append(ids, candidate.ChunkID)
	}
	hydrated, err := r.Vectors.Hydrate(ctx, ids)
	if err != nil {
		return RetrievalResult{}, err
	}
	for id, chunk := range hydrated {
		if !passesFilters(chunk, status, vigencia, tags, request.Frontmatter) {
			delete(hydrated, id)
		}
	}
	fused := fuseCandidates(semantic, lexical, graph, hydrated, lexicalTerms, effective)
	fused = CollapseParentCandidates(fused)
	if effective.HubBoostEnabled && len(fused) > 0 {
		backlinks, countErr := r.Store.BacklinkCounts(ctx)
		if countErr != nil {
			return RetrievalResult{}, countErr
		}
		ApplyHubBoost(fused, backlinks, effective.HubBoostWeight)
	}
	if effective.RecencyBoostEnabled && IsRecencyQuery(request.Question) {
		ApplyRecencyBoost(fused, effective.RecencyBoostWeight)
	}
	reranker := r.Reranker
	if reranker == nil {
		reranker = NewNoopReranker()
	}
	fused, outcome := reranker.Rerank(ctx, request.Question, fused)
	selected := Diversify(fused, request.TopK, effective, r.Vectors)
	for _, item := range selected {
		delete(item, "row")
		metadata, _ := item["metadata"].(map[string]any)
		item["tags"] = keys(metadataTags(metadata))
		item["vigencia"] = metadataVigenciaDetails(metadata)
	}
	if request.ExpandLinks && len(selected) > 0 {
		var additions []map[string]any
		if effective.GraphSearchEnabled {
			additions = GuaranteeGraphNeighbors(selected, graph, hydrated, 3)
		} else {
			additions, err = r.ExpandWikiLinks(ctx, selected, status, vigencia, tags, request.Frontmatter, 3)
			if err != nil {
				return RetrievalResult{}, err
			}
		}
		for _, item := range additions {
			metadata, _ := item["metadata"].(map[string]any)
			item["tags"] = keys(metadataTags(metadata))
			item["vigencia"] = metadataVigenciaDetails(metadata)
		}
		selected = append(selected, additions...)
	}
	r.expandSelectedSections(ctx, selected)
	trace := buildRetrievalTrace(selected, routing, semanticBackend, len(semantic), len(lexical), len(graph), len(fused))
	return RetrievalResult{Hits: selected, Rerank: outcome, Routing: routing, Trace: trace}, nil
}

func buildRetrievalTrace(selected []map[string]any, routing *RetrievalPolicy, semanticBackend string, semantic, lexical, graph, fused int) RetrievalTrace {
	trace := RetrievalTrace{Version: "1", SemanticBackend: semanticBackend, Routing: routing, Counts: RetrievalTraceCounts{Semantic: semantic, Lexical: lexical, Graph: graph, Fused: fused, Selected: len(selected)}, Candidates: make([]RetrievalTraceCandidate, 0, len(selected))}
	for _, item := range selected {
		final := numberPointer(item["rerank_combined_score"])
		if final == nil {
			final = numberPointer(item["fusion_score"])
		}
		trace.Candidates = append(trace.Candidates, RetrievalTraceCandidate{ChunkID: mustInt64(item["chunk_id"]), Path: stringValue(item["path"]), Heading: stringValue(item["heading"]), SemanticScore: numberPointer(item["semantic_score"]), LexicalScore: numberPointer(item["lexical_score"]), GraphScore: numberPointer(item["graph_score"]), FusionScore: numberPointer(item["fusion_score"]), MMRScore: numberPointer(item["mmr_score"]), RerankScore: numberPointer(item["rerank_score"]), BacklinkCount: int(floatValue(item["backlink_count"])), HubScore: numberPointer(item["hub_score"]), HubBoost: numberPointer(item["hub_boost"]), RecencyScore: numberPointer(item["recency_score"]), RecencyBoost: numberPointer(item["recency_boost"]), FinalScore: final})
	}
	return trace
}

func ApplyHubBoost(candidates []map[string]any, backlinks map[int64]int, weight float64) {
	weight = clamp01(weight)
	maximum := 0
	for _, candidate := range candidates {
		maximum = max(maximum, backlinks[mustInt64(candidate["document_id"])])
	}
	for _, candidate := range candidates {
		count := backlinks[mustInt64(candidate["document_id"])]
		hubScore, boost := 0.0, 0.0
		if maximum > 0 && count > 0 {
			hubScore = math.Log1p(float64(count)) / math.Log1p(float64(maximum))
			boost = weight * hubScore
		}
		before := clamp01(floatValue(candidate["fusion_score"]))
		candidate["fusion_score_before_hub"] = before
		candidate["backlink_count"] = count
		candidate["hub_score"] = hubScore
		candidate["hub_boost"] = boost
		candidate["fusion_score"] = clamp01(before + boost)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return floatValue(candidates[i]["fusion_score"]) > floatValue(candidates[j]["fusion_score"])
	})
}

// IsRecencyQuery deliberately uses a narrow, deterministic vocabulary. The
// mtime signal must not perturb ordinary factual questions.
func IsRecencyQuery(question string) bool {
	normalized := normalizeLexical(question)
	for _, marker := range []string{"ultimo", "ultima", "ultimos", "ultimas", "mas reciente", "reciente", "recientes", "nuevo", "nueva", "novedad", "latest", "recent"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// ApplyRecencyBoost promotes recently modified notes only after chunk-level
// fusion. Scores are normalized within the current candidate set so the boost
// remains bounded and independent from Unix timestamps.
func ApplyRecencyBoost(candidates []map[string]any, weight float64) {
	weight = clamp01(weight)
	oldest, newest := math.Inf(1), 0.0
	for _, candidate := range candidates {
		mtime := floatValue(candidate["mtime"])
		if mtime <= 0 {
			continue
		}
		oldest = math.Min(oldest, mtime)
		newest = math.Max(newest, mtime)
	}
	for _, candidate := range candidates {
		mtime := floatValue(candidate["mtime"])
		score := 0.0
		if mtime > 0 && newest > 0 {
			score = 1
			if newest > oldest {
				score = (mtime - oldest) / (newest - oldest)
			}
		}
		before := clamp01(floatValue(candidate["fusion_score"]))
		candidate["fusion_score_before_recency"] = before
		candidate["recency_score"] = score
		candidate["recency_boost"] = weight * score
		candidate["fusion_score"] = clamp01(before + weight*score)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return floatValue(candidates[i]["fusion_score"]) > floatValue(candidates[j]["fusion_score"])
	})
}

func numberPointer(value any) *float64 {
	if value == nil {
		return nil
	}
	number := floatValue(value)
	return &number
}

func fuseCandidates(semantic []SemanticCandidate, lexical []db.FTSResult, graph []GraphCandidate, hydrated map[int64]HydratedChunk, terms []string, settings config.Settings) []map[string]any {
	semanticRanks, semanticPayload := map[int64]int{}, map[int64]SemanticCandidate{}
	for index, item := range semantic {
		semanticRanks[item.ChunkID] = index + 1
		semanticPayload[item.ChunkID] = item
	}
	lexicalRanks := map[int64]int{}
	lexicalScore, lexicalCoverage := map[int64]float64{}, map[int64]float64{}
	graphRanks, graphPayload := map[int64]int{}, map[int64]GraphCandidate{}
	rank := 0
	required := RequiredLexicalMatches(len(terms))
	for _, row := range lexical {
		chunk, ok := hydrated[row.ChunkID]
		if !ok {
			continue
		}
		payload := chunkMap(chunk)
		coverage, matched := LexicalTermCoverage(payload, terms)
		if matched < required {
			continue
		}
		rank++
		lexicalRanks[row.ChunkID] = rank
		lexicalScore[row.ChunkID] = max(0, -row.BM25)
		lexicalCoverage[row.ChunkID] = coverage
	}
	graphRank := 0
	for _, candidate := range graph {
		if _, ok := hydrated[candidate.ChunkID]; !ok {
			continue
		}
		graphRank++
		graphRanks[candidate.ChunkID] = graphRank
		graphPayload[candidate.ChunkID] = candidate
	}
	semanticWeight := settings.HybridSemanticWeight
	lexicalWeight := float64(0)
	graphWeight := float64(0)
	if settings.HybridSearchEnabled {
		lexicalWeight = settings.HybridLexicalWeight
	}
	if settings.GraphSearchEnabled {
		graphWeight = settings.GraphWeight
	}
	if semanticWeight+lexicalWeight+graphWeight <= 0 {
		semanticWeight = 1
	}
	maximumRRF := (semanticWeight + lexicalWeight + graphWeight) / float64(settings.HybridRRFK+1)
	candidateIDs := map[int64]bool{}
	for id := range semanticRanks {
		candidateIDs[id] = true
	}
	for id := range lexicalRanks {
		candidateIDs[id] = true
	}
	for id := range graphRanks {
		candidateIDs[id] = true
	}
	result := []map[string]any{}
	for id := range candidateIDs {
		chunk, ok := hydrated[id]
		if !ok {
			continue
		}
		item := chunkMap(chunk)
		rawRRF := float64(0)
		semanticScore := any(nil)
		if rank, ok := semanticRanks[id]; ok {
			rawRRF += semanticWeight / float64(settings.HybridRRFK+rank)
			semanticScore = semanticPayload[id].Score
			item["row"] = semanticPayload[id].Row
		} else if row, ok := settingsRow(id, semanticPayload); ok {
			item["row"] = row
		}
		if rank, ok := lexicalRanks[id]; ok {
			rawRRF += lexicalWeight / float64(settings.HybridRRFK+rank)
		}
		if rank, ok := graphRanks[id]; ok {
			rawRRF += graphWeight / float64(settings.HybridRRFK+rank)
			if item["row"] == nil {
				item["row"] = graphPayload[id].Row
			}
		}
		var weighted, total float64
		components := []string{}
		if semanticScore != nil {
			weighted += semanticWeight * semanticScore.(float64)
			total += semanticWeight
			components = append(components, "semántica")
		}
		var lexicalValue any
		if coverage, ok := lexicalCoverage[id]; ok {
			lexicalValue = lexicalScore[id]
			weighted += lexicalWeight * coverage
			total += lexicalWeight
			components = append(components, "texto")
		}
		if candidate, ok := graphPayload[id]; ok {
			item["graph_score"] = candidate.GraphScore
			item["graph_hop"] = candidate.Hop
			item["graph_chunk_similarity"] = candidate.GraphChunkSimilarity
			weighted += graphWeight * candidate.GraphScore
			total += graphWeight
			components = append(components, "grafo")
		}
		score := float64(0)
		if total > 0 {
			score = weighted / total
		}
		item["semantic_score"] = semanticScore
		item["lexical_score"] = lexicalValue
		if coverage, ok := lexicalCoverage[id]; ok {
			item["lexical_coverage"] = coverage
		} else {
			item["lexical_coverage"] = nil
		}
		item["score"] = clamp01(score)
		item["fusion_score"] = rawRRF / maximumRRF
		if len(components) == 1 && components[0] == "semántica" {
			item["reason"] = "búsqueda semántica"
		} else if len(components) == 1 && components[0] == "texto" {
			item["reason"] = "búsqueda textual"
		} else if len(components) == 1 {
			item["reason"] = "búsqueda por grafo"
		} else {
			item["reason"] = "búsqueda híbrida (" + strings.Join(components, " + ") + ")"
		}
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := floatValue(result[i]["fusion_score"]), floatValue(result[j]["fusion_score"])
		if left == right {
			return floatValue(result[i]["score"]) > floatValue(result[j]["score"])
		}
		return left > right
	})
	return result
}

func CollapseParentCandidates(candidates []map[string]any) []map[string]any {
	result := []map[string]any{}
	byParent := map[int64]map[string]any{}
	for _, candidate := range candidates {
		parent, ok := int64Value(candidate["parent_id"])
		if !ok {
			item := cloneMap(candidate)
			item["matched_chunk_ids"] = []int64{mustInt64(candidate["chunk_id"])}
			result = append(result, item)
			continue
		}
		if existing := byParent[parent]; existing != nil {
			existing["matched_chunk_ids"] = append(existing["matched_chunk_ids"].([]int64), mustInt64(candidate["chunk_id"]))
			continue
		}
		item := cloneMap(candidate)
		item["matched_chunk_ids"] = []int64{mustInt64(candidate["chunk_id"])}
		byParent[parent] = item
		result = append(result, item)
	}
	return result
}

func Diversify(candidates []map[string]any, topK int, settings config.Settings, vectors *VectorStore) []map[string]any {
	if len(candidates) == 0 || topK <= 0 {
		return nil
	}
	if !settings.MMREnabled || len(candidates) <= 1 {
		selected := capPerDocument(candidates, topK, settings.MaxChunksPerDocument)
		for _, item := range selected {
			item["mmr_score"] = nil
		}
		return selected
	}
	rows := make([]int, len(candidates))
	for index, candidate := range candidates {
		row, ok := intValue(candidate["row"])
		if !ok {
			return capPerDocument(candidates, topK, settings.MaxChunksPerDocument)
		}
		rows[index] = row
	}
	matrix := vectors.VectorsForRows(rows)
	if len(matrix) != len(candidates) {
		return capPerDocument(candidates, topK, settings.MaxChunksPerDocument)
	}
	relevance := make([]float64, len(candidates))
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for index, candidate := range candidates {
		relevance[index] = floatValue(candidate["fusion_score"])
		minimum = min(minimum, relevance[index])
		maximum = max(maximum, relevance[index])
	}
	if maximum > minimum {
		for index := range relevance {
			relevance[index] = (relevance[index] - minimum) / (maximum - minimum)
		}
	} else {
		for index := range relevance {
			relevance[index] = 1
		}
	}
	selected := []int{}
	remaining := map[int]bool{}
	for index := range candidates {
		remaining[index] = true
	}
	perDocument := map[int64]int{}
	similarity := make([]float64, len(candidates))
	for len(remaining) > 0 && len(selected) < topK {
		pool := []int{}
		for index := range remaining {
			documentID := mustInt64(candidates[index]["document_id"])
			if perDocument[documentID] < settings.MaxChunksPerDocument {
				pool = append(pool, index)
			}
		}
		if len(pool) == 0 {
			for index := range remaining {
				pool = append(pool, index)
			}
		}
		sort.Ints(pool)
		best := pool[0]
		bestScore := math.Inf(-1)
		for _, index := range pool {
			score := settings.MMRLambda*relevance[index] - (1-settings.MMRLambda)*similarity[index]
			if score > bestScore {
				best, bestScore = index, score
			}
		}
		selected = append(selected, best)
		candidates[best]["mmr_score"] = bestScore
		delete(remaining, best)
		documentID := mustInt64(candidates[best]["document_id"])
		perDocument[documentID]++
		for index := range remaining {
			similarity[index] = max(similarity[index], dot(matrix[index], matrix[best]))
		}
	}
	result := make([]map[string]any, len(selected))
	for index, candidateIndex := range selected {
		result[index] = candidates[candidateIndex]
	}
	return result
}

func capPerDocument(candidates []map[string]any, topK, cap int) []map[string]any {
	selected, overflow := []map[string]any{}, []map[string]any{}
	counts := map[int64]int{}
	for _, candidate := range candidates {
		documentID := mustInt64(candidate["document_id"])
		if counts[documentID] >= cap {
			overflow = append(overflow, candidate)
			continue
		}
		counts[documentID]++
		selected = append(selected, candidate)
		if len(selected) >= topK {
			return selected
		}
	}
	for _, candidate := range overflow {
		if len(selected) >= topK {
			break
		}
		selected = append(selected, candidate)
	}
	return selected
}

func chunkMap(chunk HydratedChunk) map[string]any {
	return map[string]any{"chunk_id": chunk.ChunkID, "chunk_index": chunk.ChunkIndex, "heading": chunk.Heading, "heading_path": metadataHeadingPath(chunk.Metadata, chunk.ChunkIndex, chunk.ParentIndex), "outline": chunk.Metadata["_outline"], "content": chunk.Content, "matched_content": chunk.MatchedContent, "parent_id": chunk.ParentID, "parent_index": chunk.ParentIndex, "context_expanded": chunk.ContextExpanded, "embedding_dim": chunk.EmbeddingDim, "document_id": chunk.DocumentID, "path": chunk.Path, "title": chunk.Title, "mtime": chunk.MTime, "metadata": chunk.Metadata, "links": chunk.Links}
}
func passesFilters(chunk HydratedChunk, status, vigencia string, tags []string, frontmatter map[string]any) bool {
	if status != "" && metadataStatus(chunk.Metadata) != status {
		return false
	}
	actual := metadataVigencia(chunk.Metadata)
	if vigencia != "" && !(vigencia == "no_caducada" && actual != "caducada") && actual != vigencia {
		return false
	}
	available := metadataTags(chunk.Metadata)
	for _, tag := range tags {
		if !hasMatchingTag(available, tag) {
			return false
		}
	}
	return metadataMatchesFrontmatter(chunk.Metadata, frontmatter)
}

func metadataMatchesFrontmatter(metadata map[string]any, filters map[string]any) bool {
	for wantedKey, wantedValue := range filters {
		found := false
		for candidateKey, candidateValue := range metadata {
			if strings.HasPrefix(candidateKey, "_") {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(candidateKey), strings.TrimSpace(wantedKey)) && frontmatterValueMatches(candidateValue, wantedValue) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func frontmatterValueMatches(candidate, wanted any) bool {
	wantedValues := stringSlice(wanted)
	if len(wantedValues) == 0 {
		wantedValues = []string{stringify(wanted)}
	}
	candidateValues := stringSlice(candidate)
	if len(candidateValues) == 0 {
		candidateValues = []string{stringify(candidate)}
	}
	for _, expected := range wantedValues {
		matched := false
		for _, actual := range candidateValues {
			if strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(expected)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
func normalizeTagFilters(tags []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		normalized := NormalizeTag(tag)
		if normalized != "" && !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result
}
func normalizeLexical(value string) string { return accentFold.Replace(strings.ToLower(value)) }
func unicodeTokens(value string) []string {
	result := []string{}
	current := []rune{}
	flush := func() {
		if len(current) > 0 {
			result = append(result, string(current))
			current = nil
		}
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' {
			current = append(current, character)
		} else {
			flush()
		}
	}
	flush()
	return result
}
func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}
func clamp01(value float64) float64 { return max(0, min(1, value)) }
func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if pointer, ok := value.(*string); ok {
		if pointer == nil {
			return ""
		}
		return *pointer
	}
	return stringify(value)
}
func floatValue(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	}
	return 0
}
func intValue(value any) (int, bool) { number, ok := int64Value(value); return int(number), ok }
func int64Value(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, true
	case int:
		return int64(number), true
	case *int64:
		if number != nil {
			return *number, true
		}
	case *int:
		if number != nil {
			return int64(*number), true
		}
	case float64:
		return int64(number), true
	}
	return 0, false
}
func mustInt64(value any) int64 { number, _ := int64Value(value); return number }
func settingsRow(id int64, payload map[int64]SemanticCandidate) (int, bool) {
	candidate, ok := payload[id]
	return candidate.Row, ok
}
func metadataVigenciaDetails(metadata map[string]any) map[string]any {
	if metadata == nil {
		return map[string]any{}
	}
	if raw, ok := metadata["_vigencia"].(map[string]any); ok {
		return raw
	}
	return map[string]any{}
}
func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
