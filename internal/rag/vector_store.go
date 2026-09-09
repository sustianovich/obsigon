package rag

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"obsigon/internal/db"
)

type indexSignature struct {
	count, maximum int64
	fingerprint    string
}
type vectorRow struct {
	chunkID, documentID int64
	vector              []float32
	status, vigencia    string
	tags                map[string]bool
}
type SemanticCandidate struct {
	ChunkID, DocumentID int64
	Row                 int
	Score               float64
}

type ANNConfig struct {
	Enabled        bool
	Path           string
	M              int
	EFConstruction int
	EFSearch       int
}

type searchFilters struct {
	status, vigencia string
	tags             []string
	allowedDocuments map[int64]bool
}

// semanticIndex mantiene intercambiables el recorrido exacto y el HNSW. El
// exacto sigue siendo el backend predeterminado y la referencia de calidad.
type semanticIndex interface {
	Rank(rows []vectorRow, dimension int, query []float32, limit int, minSimilarity float64, filters searchFilters) []SemanticCandidate
	Backend() string
}

type exactSemanticIndex struct{ store *VectorStore }

func (index exactSemanticIndex) Rank(rows []vectorRow, _ int, query []float32, limit int, minimum float64, filters searchFilters) []SemanticCandidate {
	return index.store.rankRows(rows, query, limit, minimum, filters.status, filters.vigencia, filters.tags, filters.allowedDocuments)
}
func (exactSemanticIndex) Backend() string { return "exact" }

type annSemanticIndex struct {
	index      *hnswIndex
	rowByChunk map[int64]int
	efSearch   int
}

func (index annSemanticIndex) Rank(rows []vectorRow, _ int, query []float32, limit int, minimum float64, filters searchFilters) []SemanticCandidate {
	return rankANN(index.index, rows, index.rowByChunk, query, limit, minimum, filters.status, filters.vigencia, filters.tags, filters.allowedDocuments, index.efSearch)
}
func (annSemanticIndex) Backend() string { return "ann" }

type VectorStore struct {
	store        *db.Store
	workers      int
	mu           sync.RWMutex
	reloading    bool
	reloadDone   chan struct{}
	lastLoadErr  error
	signature    indexSignature
	loaded       bool
	dimension    int
	rows         []vectorRow
	rowByChunk   map[int64]int
	tagDocuments map[string]map[int64]bool
	annConfig    ANNConfig
	ann          *hnswIndex
	annError     string
	beforeLoad   func()
}

func NewVectorStore(store *db.Store, workers int, options ...ANNConfig) *VectorStore {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	annConfig := ANNConfig{M: 16, EFConstruction: 200, EFSearch: 64}
	if len(options) > 0 {
		annConfig = options[0]
		annConfig.M = max(4, annConfig.M)
		annConfig.EFConstruction = max(annConfig.M, annConfig.EFConstruction)
		annConfig.EFSearch = max(8, annConfig.EFSearch)
	}
	return &VectorStore{store: store, workers: workers, annConfig: annConfig, signature: indexSignature{count: -1, maximum: -1}, rowByChunk: map[int64]int{}}
}
func (v *VectorStore) Invalidate() {
	v.mu.Lock()
	v.loaded = false
	v.signature = indexSignature{count: -1, maximum: -1}
	v.mu.Unlock()
}

func (v *VectorStore) ensureLoaded(ctx context.Context) error {
	v.mu.RLock()
	if v.loaded && v.reloading {
		v.mu.RUnlock()
		return nil
	}
	v.mu.RUnlock()

	signature, err := v.readSignature(ctx, v.store.ReadDB())
	if err != nil {
		return err
	}
	v.mu.Lock()
	if v.loaded && signature == v.signature {
		v.mu.Unlock()
		return nil
	}
	if v.reloading {
		if v.loaded {
			v.mu.Unlock()
			return nil
		}
		done := v.reloadDone
		v.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			v.mu.RLock()
			err := v.lastLoadErr
			v.mu.RUnlock()
			return err
		}
	}
	v.reloading = true
	v.reloadDone = make(chan struct{})
	done := v.reloadDone
	var previous *vectorSnapshot
	var previousANN *hnswIndex
	if len(v.rows) > 0 {
		previous = &vectorSnapshot{signature: v.signature, dimension: v.dimension, rows: v.rows, rowByChunk: v.rowByChunk, tagDocuments: v.tagDocuments}
		previousANN = v.ann
	}
	v.mu.Unlock()

	if v.beforeLoad != nil {
		v.beforeLoad()
	}
	snapshot, loadErr := v.buildSnapshot(ctx, previous)
	if loadErr == nil && v.annConfig.Enabled {
		ann, annErr := v.prepareANN(snapshot, previousANN)
		snapshot.ann = ann
		if annErr != nil {
			snapshot.annError = annErr.Error()
		}
	}
	v.mu.Lock()
	if loadErr == nil {
		v.rows = snapshot.rows
		v.rowByChunk = snapshot.rowByChunk
		v.tagDocuments = snapshot.tagDocuments
		v.dimension = snapshot.dimension
		v.signature = snapshot.signature
		v.ann = snapshot.ann
		v.annError = snapshot.annError
		v.loaded = true
	}
	v.lastLoadErr = loadErr
	v.reloading = false
	close(done)
	v.mu.Unlock()
	return loadErr
}

type vectorSnapshot struct {
	signature    indexSignature
	dimension    int
	rows         []vectorRow
	rowByChunk   map[int64]int
	tagDocuments map[string]map[int64]bool
	ann          *hnswIndex
	annError     string
}

type signatureQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (v *VectorStore) readSignature(ctx context.Context, queryer signatureQuerier) (indexSignature, error) {
	var signature indexSignature
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM chunks`).Scan(&signature.count, &signature.maximum); err != nil {
		return indexSignature{}, err
	}
	_ = queryer.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, IndexFingerprintKey).Scan(&signature.fingerprint)
	return signature, nil
}

// buildSnapshot publica un snapshot nuevo fuera del lock. Si ya hay uno cargado
// y la dimensión no ha cambiado, sólo decodifica los fragmentos añadidos y
// descarta los eliminados; en cualquier otro caso relee todo. Los id de
// fragmento son inmutables (toda reindexación de un documento inserta filas
// nuevas, nunca muta una existente), así que la identidad por id basta para el
// delta.
func (v *VectorStore) buildSnapshot(ctx context.Context, previous *vectorSnapshot) (vectorSnapshot, error) {
	tx, err := v.store.ReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return vectorSnapshot{}, err
	}
	defer tx.Rollback()
	signature, err := v.readSignature(ctx, tx)
	if err != nil {
		return vectorSnapshot{}, err
	}
	dimension, err := dominantDimension(ctx, tx)
	if err != nil {
		return vectorSnapshot{}, err
	}
	var snapshot vectorSnapshot
	if previous != nil && previous.dimension != 0 && dimension == previous.dimension {
		snapshot, err = v.incrementalSnapshot(ctx, tx, previous, signature, dimension)
	} else {
		snapshot, err = fullSnapshot(ctx, tx, signature)
	}
	if err != nil {
		return vectorSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return vectorSnapshot{}, err
	}
	return snapshot, nil
}

// fullSnapshot lee todos los fragmentos y elige la dimensión mayoritaria: mayor
// recuento y, en empate, la dimensión menor.
func fullSnapshot(ctx context.Context, tx *sql.Tx, signature indexSignature) (vectorSnapshot, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.embedding,c.embedding_dim,d.id,d.metadata_json FROM chunks c JOIN documents d ON d.id=c.document_id ORDER BY c.id`)
	if err != nil {
		return vectorSnapshot{}, err
	}
	defer rows.Close()
	type rawRow struct {
		chunkID, documentID int64
		embedding           []byte
		dimension           int
		metadata            string
	}
	var raw []rawRow
	counts, order := map[int]int{}, []int{}
	for rows.Next() {
		var item rawRow
		if err := rows.Scan(&item.chunkID, &item.embedding, &item.dimension, &item.documentID, &item.metadata); err != nil {
			return vectorSnapshot{}, err
		}
		raw = append(raw, item)
		if _, ok := counts[item.dimension]; !ok {
			order = append(order, item.dimension)
		}
		counts[item.dimension]++
	}
	if err := rows.Err(); err != nil {
		return vectorSnapshot{}, err
	}
	dimension := 0
	for _, candidate := range order {
		if dimension == 0 || counts[candidate] > counts[dimension] {
			dimension = candidate
		}
	}
	loaded := make([]vectorRow, 0, len(raw))
	rowByChunk := map[int64]int{}
	for _, item := range raw {
		if item.dimension != dimension || len(item.embedding) != dimension*4 {
			continue
		}
		row := decodeVectorRow(item.chunkID, item.documentID, item.embedding, item.metadata)
		rowByChunk[item.chunkID] = len(loaded)
		loaded = append(loaded, row)
	}
	if err := rows.Close(); err != nil {
		return vectorSnapshot{}, err
	}
	return vectorSnapshot{signature: signature, dimension: dimension, rows: loaded, rowByChunk: rowByChunk, tagDocuments: tagDocumentIndex(loaded)}, nil
}

// incrementalSnapshot conserva las filas cuyo id de fragmento sigue presente y
// sólo decodifica los fragmentos nuevos. El orden por id ascendente se mantiene
// mediante una fusión de las filas conservadas y las nuevas.
func (v *VectorStore) incrementalSnapshot(ctx context.Context, tx *sql.Tx, previous *vectorSnapshot, signature indexSignature, dimension int) (vectorSnapshot, error) {
	currentIDs, err := chunkIDs(ctx, tx)
	if err != nil {
		return vectorSnapshot{}, err
	}
	current := make(map[int64]struct{}, len(currentIDs))
	for _, id := range currentIDs {
		current[id] = struct{}{}
	}
	kept := make([]vectorRow, 0, len(previous.rows))
	for _, row := range previous.rows {
		if _, ok := current[row.chunkID]; ok {
			kept = append(kept, row)
		}
	}
	if len(kept) == 0 {
		return fullSnapshot(ctx, tx, signature)
	}
	added := make([]int64, 0)
	for _, id := range currentIDs {
		if _, ok := previous.rowByChunk[id]; !ok {
			added = append(added, id)
		}
	}
	if len(added) == 0 && len(kept) == len(previous.rows) {
		return vectorSnapshot{signature: signature, dimension: previous.dimension, rows: previous.rows, rowByChunk: previous.rowByChunk, tagDocuments: previous.tagDocuments}, nil
	}
	fresh, err := loadVectorRows(ctx, tx, dimension, added)
	if err != nil {
		return vectorSnapshot{}, err
	}
	merged := mergeVectorRows(kept, fresh)
	rowByChunk := make(map[int64]int, len(merged))
	for index, row := range merged {
		rowByChunk[row.chunkID] = index
	}
	return vectorSnapshot{signature: signature, dimension: dimension, rows: merged, rowByChunk: rowByChunk, tagDocuments: tagDocumentIndex(merged)}, nil
}

func dominantDimension(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT embedding_dim,COUNT(*) FROM chunks GROUP BY embedding_dim`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	best, bestCount := 0, -1
	for rows.Next() {
		var dimension, count int
		if err := rows.Scan(&dimension, &count); err != nil {
			return 0, err
		}
		if count > bestCount || (count == bestCount && dimension < best) {
			best, bestCount = dimension, count
		}
	}
	return best, rows.Err()
}

func chunkIDs(ctx context.Context, tx *sql.Tx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM chunks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// loadVectorRows decodifica los fragmentos indicados en lotes y devuelve las
// filas ordenadas por id ascendente (los ids de entrada ya vienen ordenados).
func loadVectorRows(ctx context.Context, tx *sql.Tx, dimension int, ids []int64) ([]vectorRow, error) {
	result := make([]vectorRow, 0, len(ids))
	const batch = 400
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		slice := ids[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(slice)), ",")
		args := make([]any, len(slice))
		for index, id := range slice {
			args[index] = id
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.id,c.embedding,c.embedding_dim,d.id,d.metadata_json FROM chunks c JOIN documents d ON d.id=c.document_id WHERE c.id IN (`+placeholders+`) ORDER BY c.id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var chunkID, documentID int64
			var embedding []byte
			var dim int
			var metadata string
			if err := rows.Scan(&chunkID, &embedding, &dim, &documentID, &metadata); err != nil {
				rows.Close()
				return nil, err
			}
			if dim != dimension || len(embedding) != dimension*4 {
				continue
			}
			result = append(result, decodeVectorRow(chunkID, documentID, embedding, metadata))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func decodeVectorRow(chunkID, documentID int64, embedding []byte, metadataJSON string) vectorRow {
	vector := decodeFloat32(embedding)
	normalizeVector(vector)
	metadata := map[string]any{}
	_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	tags := metadataTags(metadata)
	return vectorRow{chunkID: chunkID, documentID: documentID, vector: vector, status: metadataStatus(metadata), vigencia: metadataVigencia(metadata), tags: tags}
}

// mergeVectorRows fusiona dos listas ya ordenadas por id ascendente.
func mergeVectorRows(left, right []vectorRow) []vectorRow {
	out := make([]vectorRow, 0, len(left)+len(right))
	i, j := 0, 0
	for i < len(left) && j < len(right) {
		if left[i].chunkID <= right[j].chunkID {
			out = append(out, left[i])
			i++
		} else {
			out = append(out, right[j])
			j++
		}
	}
	out = append(out, left[i:]...)
	out = append(out, right[j:]...)
	return out
}

func tagDocumentIndex(rows []vectorRow) map[string]map[int64]bool {
	result := map[string]map[int64]bool{}
	for _, row := range rows {
		for tag := range row.tags {
			if result[tag] == nil {
				result[tag] = map[int64]bool{}
			}
			result[tag][row.documentID] = true
		}
	}
	return result
}

func (v *VectorStore) Search(ctx context.Context, query []float32, limit int, minSimilarity float64, status, vigencia string, tags []string) ([]SemanticCandidate, error) {
	return v.SearchDocuments(ctx, query, limit, minSimilarity, status, vigencia, tags, nil)
}

func (v *VectorStore) SearchDocuments(ctx context.Context, query []float32, limit int, minSimilarity float64, status, vigencia string, tags []string, allowedDocuments map[int64]bool) ([]SemanticCandidate, error) {
	results, _, err := v.SearchDocumentsWithBackend(ctx, query, limit, minSimilarity, status, vigencia, tags, allowedDocuments)
	return results, err
}

func (v *VectorStore) SearchDocumentsWithBackend(ctx context.Context, query []float32, limit int, minSimilarity float64, status, vigencia string, tags []string, allowedDocuments map[int64]bool) ([]SemanticCandidate, string, error) {
	if err := v.ensureLoaded(ctx); err != nil {
		return nil, "exact", err
	}
	v.mu.RLock()
	rows, rowByChunk, dimension, ann := v.rows, v.rowByChunk, v.dimension, v.ann
	v.mu.RUnlock()
	if len(rows) == 0 || limit <= 0 || len(query) != dimension {
		return nil, backendName(ann), nil
	}
	normalized := append([]float32(nil), query...)
	if normalizeVector(normalized) == 0 {
		return nil, backendName(ann), nil
	}
	filters := searchFilters{status: status, vigencia: vigencia, tags: tags, allowedDocuments: allowedDocuments}
	var semantic semanticIndex = exactSemanticIndex{store: v}
	if ann != nil {
		semantic = annSemanticIndex{index: ann, rowByChunk: rowByChunk, efSearch: v.annConfig.EFSearch}
	}
	return semantic.Rank(rows, dimension, normalized, limit, minSimilarity, filters), semantic.Backend(), nil
}

// rankRows recorre las filas por rangos contiguos, uno por worker, y sólo
// calcula el coseno de las filas que superan los filtros baratos. Evita el
// vector completo de puntuaciones y el reparto elemento a elemento por canal
// del código anterior, y conserva el orden estable por índice de fila.
func (v *VectorStore) rankRows(rows []vectorRow, normalized []float32, limit int, minSimilarity float64, status, vigencia string, tags []string, allowedDocuments map[int64]bool) []SemanticCandidate {
	requestedStatus := strings.ToLower(strings.TrimSpace(status))
	requestedVigencia := strings.ToLower(strings.TrimSpace(vigencia))
	workers := v.workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(rows) {
		workers = len(rows)
	}
	span := (len(rows) + workers - 1) / workers
	partials := make([][]SemanticCandidate, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		start := worker * span
		if start >= len(rows) {
			break
		}
		end := start + span
		if end > len(rows) {
			end = len(rows)
		}
		wait.Add(1)
		go func(worker, start, end int) {
			defer wait.Done()
			local := make([]SemanticCandidate, 0, 16)
			for index := start; index < end; index++ {
				row := rows[index]
				if allowedDocuments != nil && !allowedDocuments[row.documentID] {
					continue
				}
				if requestedStatus != "" && row.status != requestedStatus {
					continue
				}
				if requestedVigencia != "" && !(requestedVigencia == "no_caducada" && row.vigencia != "caducada") && row.vigencia != requestedVigencia {
					continue
				}
				if !rowMatchesTags(row.tags, tags) {
					continue
				}
				score := dot(row.vector, normalized)
				if score < minSimilarity {
					continue
				}
				local = append(local, SemanticCandidate{ChunkID: row.chunkID, DocumentID: row.documentID, Row: index, Score: score})
			}
			partials[worker] = local
		}(worker, start, end)
	}
	wait.Wait()
	results := make([]SemanticCandidate, 0)
	for _, part := range partials {
		results = append(results, part...)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Row < results[j].Row
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func rowMatchesTags(available map[string]bool, wanted []string) bool {
	for _, tag := range wanted {
		if !hasMatchingTag(available, tag) {
			return false
		}
	}
	return true
}

func hasMatchingTag(available map[string]bool, wanted string) bool {
	wanted = NormalizeTag(wanted)
	for candidate := range available {
		if candidate == wanted || strings.HasPrefix(candidate, wanted+"/") {
			return true
		}
	}
	return false
}

func (v *VectorStore) VectorsForRows(indices []int) [][]float32 {
	v.mu.RLock()
	rows := v.rows
	v.mu.RUnlock()
	result := make([][]float32, 0, len(indices))
	for _, index := range indices {
		if index < 0 || index >= len(rows) {
			return nil
		}
		result = append(result, append([]float32(nil), rows[index].vector...))
	}
	return result
}
func (v *VectorStore) RowForChunk(chunkID int64) (int, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	row, ok := v.rowByChunk[chunkID]
	return row, ok
}

// BestChunksForDocuments elige el fragmento más parecido a la consulta por
// cada nota alcanzada por el grafo. No asume que chunk_index=0 sea el mejor.
func (v *VectorStore) BestChunksForDocuments(ctx context.Context, query []float32, documentScores map[int64]float64) ([]GraphCandidate, error) {
	if err := v.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	rows, dimension := v.rows, v.dimension
	v.mu.RUnlock()
	if len(query) != dimension || len(documentScores) == 0 {
		return nil, nil
	}
	normalized := append([]float32(nil), query...)
	if normalizeVector(normalized) == 0 {
		return nil, nil
	}
	best := map[int64]GraphCandidate{}
	for rowIndex, row := range rows {
		graphScore, wanted := documentScores[row.documentID]
		if !wanted {
			continue
		}
		similarity := dot(row.vector, normalized)
		current, exists := best[row.documentID]
		if !exists || similarity > current.GraphChunkSimilarity {
			best[row.documentID] = GraphCandidate{ChunkID: row.chunkID, DocumentID: row.documentID, Row: rowIndex, GraphScore: graphScore, GraphChunkSimilarity: similarity}
		}
	}
	result := make([]GraphCandidate, 0, len(best))
	for _, item := range best {
		result = append(result, item)
	}
	return result, nil
}

type HydratedChunk struct {
	ChunkID          int64 `json:"chunk_id"`
	ChunkIndex       int   `json:"chunk_index"`
	Heading, Content string
	MatchedContent   *string
	ParentID         *int64
	ParentIndex      *int
	ContextExpanded  bool
	EmbeddingDim     int
	DocumentID       int64
	Path, Title      string
	MTime            float64
	Metadata         map[string]any
	Links            []WikiLink
}

func (v *VectorStore) Hydrate(ctx context.Context, chunkIDs []int64) (map[int64]HydratedChunk, error) {
	if len(chunkIDs) == 0 {
		return map[int64]HydratedChunk{}, nil
	}
	unique := []int64{}
	seen := map[int64]bool{}
	for _, id := range chunkIDs {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, len(unique))
	for index, id := range unique {
		args[index] = id
	}
	rows, err := v.store.ReadDB().QueryContext(ctx, `SELECT c.id,c.chunk_index,c.heading,c.content,c.parent_id,c.embedding_dim,p.parent_index,p.content,d.id,d.path,d.title,d.mtime,d.metadata_json,d.links_json FROM chunks c JOIN documents d ON d.id=c.document_id LEFT JOIN chunk_parents p ON p.id=c.parent_id WHERE c.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]HydratedChunk{}
	for rows.Next() {
		var item HydratedChunk
		var childContent, metadataJSON, linksJSON string
		var parentID sql.NullInt64
		var parentIndex sql.NullInt64
		var parentContent sql.NullString
		if err := rows.Scan(&item.ChunkID, &item.ChunkIndex, &item.Heading, &childContent, &parentID, &item.EmbeddingDim, &parentIndex, &parentContent, &item.DocumentID, &item.Path, &item.Title, &item.MTime, &metadataJSON, &linksJSON); err != nil {
			return nil, err
		}
		item.Content = childContent
		if parentID.Valid {
			item.ParentID = &parentID.Int64
			value := int(parentIndex.Int64)
			item.ParentIndex = &value
			item.ContextExpanded = true
			if parentContent.Valid {
				item.Content = parentContent.String
			}
			matched := childContent
			item.MatchedContent = &matched
		}
		item.Metadata = map[string]any{}
		_ = json.Unmarshal([]byte(metadataJSON), &item.Metadata)
		_ = json.Unmarshal([]byte(linksJSON), &item.Links)
		result[item.ChunkID] = item
	}
	return result, rows.Err()
}

type VectorStats struct {
	LoadedChunks, Dimension int
	MemoryMB                float64
	FingerprintKnown        bool
	Vigencias, Tags         []string
	TagCounts               []map[string]any
	SemanticBackend         string  `json:"semantic_backend"`
	ANNEnabled              bool    `json:"ann_enabled"`
	ANNAvailable            bool    `json:"ann_available"`
	ANNMemoryMB             float64 `json:"ann_memory_mb"`
	ANNError                string  `json:"ann_error,omitempty"`
}

func (v *VectorStore) Stats(ctx context.Context) (VectorStats, error) {
	if err := v.ensureLoaded(ctx); err != nil {
		return VectorStats{}, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	vigencias, tags := map[string]bool{}, map[string]bool{}
	for _, row := range v.rows {
		if row.vigencia != "" {
			vigencias[row.vigencia] = true
		}
		for tag := range row.tags {
			tags[tag] = true
		}
	}
	annMemory := float64(0)
	if v.ann != nil {
		annMemory = math.Round(float64(v.ann.memoryBytes())/(1024*1024)*100) / 100
	}
	result := VectorStats{LoadedChunks: len(v.rows), Dimension: v.dimension, MemoryMB: math.Round(float64(len(v.rows)*v.dimension*4)/(1024*1024)*100) / 100, FingerprintKnown: v.signature.fingerprint != "", Vigencias: sortedKeys(vigencias), Tags: sortedKeys(tags), SemanticBackend: backendName(v.ann), ANNEnabled: v.annConfig.Enabled, ANNAvailable: v.ann != nil, ANNMemoryMB: annMemory, ANNError: v.annError}
	for _, tag := range result.Tags {
		result.TagCounts = append(result.TagCounts, map[string]any{"tag": tag, "documents": len(v.tagDocuments[tag])})
	}
	return result, nil
}

func decodeFloat32(data []byte) []float32 {
	result := make([]float32, len(data)/4)
	for index := range result {
		result[index] = math.Float32frombits(binary.LittleEndian.Uint32(data[index*4:]))
	}
	return result
}
func normalizeVector(vector []float32) float64 {
	sum := 0.0
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return 0
	}
	for index := range vector {
		vector[index] = float32(float64(vector[index]) / norm)
	}
	return norm
}
func dot(left, right []float32) float64 {
	sum := float64(0)
	for index := range left {
		sum += float64(left[index]) * float64(right[index])
	}
	return sum
}
func metadataStatus(metadata map[string]any) string {
	value := metadata["estado"]
	if value == nil {
		value = metadata["status"]
	}
	if value == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringify(value)))
}
func metadataVigencia(metadata map[string]any) string {
	if raw, ok := metadata["_vigencia"].(map[string]any); ok {
		return strings.TrimSpace(stringify(raw["estado_temporal"]))
	}
	return "desconocida"
}
func metadataTags(metadata map[string]any) map[string]bool {
	result := map[string]bool{}
	switch values := metadata["_tags"].(type) {
	case []any:
		for _, value := range values {
			result[NormalizeTag(stringify(value))] = true
		}
	case []string:
		for _, value := range values {
			result[NormalizeTag(value)] = true
		}
	}
	delete(result, "")
	return result
}

func metadataHeadingPath(metadata map[string]any, chunkIndex int, parentIndex *int) []string {
	key, bucket := strconv.Itoa(chunkIndex), "_chunk_heading_paths"
	if parentIndex != nil {
		key, bucket = strconv.Itoa(*parentIndex), "_parent_heading_paths"
	}
	paths, _ := metadata[bucket].(map[string]any)
	return stringSlice(paths[key])
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(stringify(item)); text != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}
func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
