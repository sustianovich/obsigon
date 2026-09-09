package rag

import (
	"container/heap"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	hnswFileMagic   = "OBSIGON-HNSW"
	hnswFileVersion = 1
	tombstoneLimit  = 0.20
)

var errANNStale = errors.New("el índice ANN persistido no corresponde al snapshot activo")

type hnswNode struct {
	ChunkID   int64
	Vector    []float32
	Level     int
	Neighbors [][]int
	Deleted   bool
}

type hnswIndex struct {
	Dimension      int
	M              int
	EFConstruction int
	EntryPoint     int
	MaxLevel       int
	Nodes          []hnswNode
	byChunk        map[int64]int
}

type persistedHNSW struct {
	Magic          string
	Version        int
	Count          int64
	Maximum        int64
	Fingerprint    string
	Dimension      int
	M              int
	EFConstruction int
	EntryPoint     int
	MaxLevel       int
	Nodes          []hnswNode
}

func backendName(index *hnswIndex) string {
	if index != nil {
		return "ann"
	}
	return "exact"
}

func (v *VectorStore) prepareANN(snapshot vectorSnapshot, previous *hnswIndex) (*hnswIndex, error) {
	config := v.annConfig
	if strings.TrimSpace(config.Path) == "" {
		return nil, errors.New("RAG_ANN requiere una ruta de persistencia")
	}
	var index *hnswIndex
	if previous != nil && previous.Dimension == snapshot.dimension && previous.M == config.M && previous.EFConstruction == config.EFConstruction {
		index = previous.clone()
		index.patch(snapshot.rows)
	} else {
		loaded, err := loadHNSW(config.Path, snapshot, config)
		switch {
		case err == nil:
			index = loaded
		case errors.Is(err, os.ErrNotExist), errors.Is(err, errANNStale):
			index = buildHNSW(snapshot.rows, snapshot.dimension, config.M, config.EFConstruction)
		default:
			return nil, fmt.Errorf("no se pudo cargar el índice ANN; se usa búsqueda exacta: %w", err)
		}
	}
	if err := saveHNSW(config.Path, snapshot.signature, index); err != nil {
		return nil, fmt.Errorf("no se pudo persistir el índice ANN; se usa búsqueda exacta: %w", err)
	}
	return index, nil
}

func buildHNSW(rows []vectorRow, dimension, m, efConstruction int) *hnswIndex {
	index := &hnswIndex{Dimension: dimension, M: m, EFConstruction: efConstruction, EntryPoint: -1, MaxLevel: -1, byChunk: map[int64]int{}}
	for _, row := range rows {
		index.insert(row.chunkID, row.vector)
	}
	return index
}

func (index *hnswIndex) clone() *hnswIndex {
	copyIndex := &hnswIndex{Dimension: index.Dimension, M: index.M, EFConstruction: index.EFConstruction, EntryPoint: index.EntryPoint, MaxLevel: index.MaxLevel, Nodes: make([]hnswNode, len(index.Nodes)), byChunk: make(map[int64]int, len(index.byChunk))}
	for i, node := range index.Nodes {
		copyIndex.Nodes[i] = hnswNode{ChunkID: node.ChunkID, Vector: append([]float32(nil), node.Vector...), Level: node.Level, Deleted: node.Deleted, Neighbors: make([][]int, len(node.Neighbors))}
		for level := range node.Neighbors {
			copyIndex.Nodes[i].Neighbors[level] = append([]int(nil), node.Neighbors[level]...)
		}
		copyIndex.byChunk[node.ChunkID] = i
	}
	return copyIndex
}

// patch conserva el grafo existente, marca bajas como tombstones e inserta los
// nuevos chunk_id. Si la fragmentación supera el 20 %, compacta reconstruyendo.
func (index *hnswIndex) patch(rows []vectorRow) {
	active := make(map[int64]vectorRow, len(rows))
	for _, row := range rows {
		active[row.chunkID] = row
	}
	deleted := 0
	for nodeIndex := range index.Nodes {
		_, exists := active[index.Nodes[nodeIndex].ChunkID]
		index.Nodes[nodeIndex].Deleted = !exists
		if !exists {
			deleted++
		}
	}
	for _, row := range rows {
		if nodeIndex, exists := index.byChunk[row.chunkID]; exists {
			index.Nodes[nodeIndex].Deleted = false
			continue
		}
		index.insert(row.chunkID, row.vector)
	}
	if len(index.Nodes) > 0 && float64(deleted)/float64(len(index.Nodes)) > tombstoneLimit {
		fresh := buildHNSW(rows, index.Dimension, index.M, index.EFConstruction)
		*index = *fresh
	}
}

func (index *hnswIndex) insert(chunkID int64, vector []float32) {
	level := deterministicLevel(chunkID, index.M)
	nodeIndex := len(index.Nodes)
	index.Nodes = append(index.Nodes, hnswNode{ChunkID: chunkID, Vector: append([]float32(nil), vector...), Level: level, Neighbors: make([][]int, level+1)})
	index.byChunk[chunkID] = nodeIndex
	if index.EntryPoint < 0 {
		index.EntryPoint, index.MaxLevel = nodeIndex, level
		return
	}
	entry := index.EntryPoint
	for layer := index.MaxLevel; layer > level; layer-- {
		entry = index.greedy(vector, entry, layer)
	}
	for layer := min(level, index.MaxLevel); layer >= 0; layer-- {
		candidates := index.searchLayer(vector, []int{entry}, index.EFConstruction, layer)
		selected := make([]int, 0, index.M)
		for _, candidate := range candidates {
			if candidate.index == nodeIndex || index.Nodes[candidate.index].Deleted {
				continue
			}
			selected = append(selected, candidate.index)
			if len(selected) == index.M {
				break
			}
		}
		index.Nodes[nodeIndex].Neighbors[layer] = append(index.Nodes[nodeIndex].Neighbors[layer], selected...)
		for _, neighbor := range selected {
			index.Nodes[neighbor].Neighbors[layer] = appendUniqueInt(index.Nodes[neighbor].Neighbors[layer], nodeIndex)
			index.trimNeighbors(neighbor, layer)
		}
		if len(candidates) > 0 {
			entry = candidates[0].index
		}
	}
	if level > index.MaxLevel {
		index.EntryPoint, index.MaxLevel = nodeIndex, level
	}
}

func deterministicLevel(chunkID int64, m int) int {
	x := uint64(chunkID) + 0x9e3779b97f4a7c15
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	level := 0
	for level < 24 && x%uint64(max(2, m)) == 0 {
		level++
		x /= uint64(max(2, m))
	}
	return level
}

func (index *hnswIndex) greedy(query []float32, entry, level int) int {
	best := entry
	bestScore := dot(query, index.Nodes[best].Vector)
	for {
		changed := false
		if level >= len(index.Nodes[best].Neighbors) {
			return best
		}
		for _, neighbor := range index.Nodes[best].Neighbors[level] {
			score := dot(query, index.Nodes[neighbor].Vector)
			if score > bestScore {
				best, bestScore, changed = neighbor, score, true
			}
		}
		if !changed {
			return best
		}
	}
}

func (index *hnswIndex) trimNeighbors(nodeIndex, level int) {
	neighbors := index.Nodes[nodeIndex].Neighbors[level]
	if len(neighbors) <= index.M {
		return
	}
	vector := index.Nodes[nodeIndex].Vector
	sort.Slice(neighbors, func(i, j int) bool {
		left, right := dot(vector, index.Nodes[neighbors[i]].Vector), dot(vector, index.Nodes[neighbors[j]].Vector)
		if left == right {
			return neighbors[i] < neighbors[j]
		}
		return left > right
	})
	index.Nodes[nodeIndex].Neighbors[level] = neighbors[:index.M]
}

type scoredNode struct {
	index int
	score float64
}

type minScoreHeap []scoredNode

func (h minScoreHeap) Len() int           { return len(h) }
func (h minScoreHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h minScoreHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minScoreHeap) Push(value any)    { *h = append(*h, value.(scoredNode)) }
func (h *minScoreHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

type maxScoreHeap []scoredNode

func (h maxScoreHeap) Len() int           { return len(h) }
func (h maxScoreHeap) Less(i, j int) bool { return h[i].score > h[j].score }
func (h maxScoreHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxScoreHeap) Push(value any)    { *h = append(*h, value.(scoredNode)) }
func (h *maxScoreHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func (index *hnswIndex) searchLayer(query []float32, entries []int, ef, level int) []scoredNode {
	if len(index.Nodes) == 0 || ef <= 0 {
		return nil
	}
	visited := make(map[int]struct{}, ef*4)
	candidates := &maxScoreHeap{}
	results := &minScoreHeap{}
	heap.Init(candidates)
	heap.Init(results)
	for _, entry := range entries {
		if entry < 0 || entry >= len(index.Nodes) {
			continue
		}
		if _, exists := visited[entry]; exists {
			continue
		}
		visited[entry] = struct{}{}
		item := scoredNode{index: entry, score: dot(query, index.Nodes[entry].Vector)}
		heap.Push(candidates, item)
		heap.Push(results, item)
	}
	for candidates.Len() > 0 {
		current := heap.Pop(candidates).(scoredNode)
		if results.Len() >= ef && current.score < (*results)[0].score {
			break
		}
		if level >= len(index.Nodes[current.index].Neighbors) {
			continue
		}
		for _, neighbor := range index.Nodes[current.index].Neighbors[level] {
			if _, exists := visited[neighbor]; exists {
				continue
			}
			visited[neighbor] = struct{}{}
			item := scoredNode{index: neighbor, score: dot(query, index.Nodes[neighbor].Vector)}
			if results.Len() < ef || item.score > (*results)[0].score {
				heap.Push(candidates, item)
				heap.Push(results, item)
				if results.Len() > ef {
					heap.Pop(results)
				}
			}
		}
	}
	ordered := make([]scoredNode, results.Len())
	copy(ordered, *results)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			return ordered[i].index < ordered[j].index
		}
		return ordered[i].score > ordered[j].score
	})
	return ordered
}

func (index *hnswIndex) search(query []float32, ef int) []int64 {
	if index.EntryPoint < 0 || len(index.Nodes) == 0 {
		return nil
	}
	entry := index.EntryPoint
	for level := index.MaxLevel; level > 0; level-- {
		entry = index.greedy(query, entry, level)
	}
	candidates := index.searchLayer(query, []int{entry}, min(max(1, ef), len(index.Nodes)), 0)
	result := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if !index.Nodes[candidate.index].Deleted {
			result = append(result, index.Nodes[candidate.index].ChunkID)
		}
	}
	return result
}

func rankANN(index *hnswIndex, rows []vectorRow, rowByChunk map[int64]int, query []float32, limit int, minimum float64, status, vigencia string, tags []string, allowedDocuments map[int64]bool, efSearch int) []SemanticCandidate {
	requestedStatus := strings.ToLower(strings.TrimSpace(status))
	requestedVigencia := strings.ToLower(strings.TrimSpace(vigencia))
	filtered := allowedDocuments != nil || requestedStatus != "" || requestedVigencia != "" || len(tags) > 0
	ef := min(len(index.Nodes), max(efSearch, limit*4))
	seen := map[int64]struct{}{}
	result := make([]SemanticCandidate, 0, limit)
	for {
		ids := index.search(query, ef)
		lastScore := 1.0
		for _, chunkID := range ids {
			if _, exists := seen[chunkID]; exists {
				continue
			}
			seen[chunkID] = struct{}{}
			rowIndex, exists := rowByChunk[chunkID]
			if !exists {
				continue
			}
			row := rows[rowIndex]
			score := dot(row.vector, query)
			lastScore = score
			if !matchesSemanticFilters(row, requestedStatus, requestedVigencia, tags, allowedDocuments) || score < minimum {
				continue
			}
			result = append(result, SemanticCandidate{ChunkID: row.chunkID, DocumentID: row.documentID, Row: rowIndex, Score: score})
		}
		if len(result) >= limit || ef >= len(index.Nodes) || (!filtered && len(ids) >= ef && lastScore < minimum) {
			break
		}
		ef = min(len(index.Nodes), max(ef+1, ef*2))
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return result[i].Row < result[j].Row
		}
		return result[i].Score > result[j].Score
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func matchesSemanticFilters(row vectorRow, status, vigencia string, tags []string, allowedDocuments map[int64]bool) bool {
	if allowedDocuments != nil && !allowedDocuments[row.documentID] {
		return false
	}
	if status != "" && row.status != status {
		return false
	}
	if vigencia != "" && !(vigencia == "no_caducada" && row.vigencia != "caducada") && row.vigencia != vigencia {
		return false
	}
	return rowMatchesTags(row.tags, tags)
}

func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func loadHNSW(path string, snapshot vectorSnapshot, config ANNConfig) (*hnswIndex, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var persisted persistedHNSW
	if err := gob.NewDecoder(file).Decode(&persisted); err != nil {
		return nil, fmt.Errorf("archivo corrupto: %w", err)
	}
	if persisted.Magic != hnswFileMagic || persisted.Version != hnswFileVersion {
		return nil, errors.New("formato ANN desconocido o corrupto")
	}
	if persisted.Count != snapshot.signature.count || persisted.Maximum != snapshot.signature.maximum || persisted.Fingerprint != snapshot.signature.fingerprint || persisted.Dimension != snapshot.dimension || persisted.M != config.M || persisted.EFConstruction != config.EFConstruction {
		return nil, errANNStale
	}
	index := &hnswIndex{Dimension: persisted.Dimension, M: persisted.M, EFConstruction: persisted.EFConstruction, EntryPoint: persisted.EntryPoint, MaxLevel: persisted.MaxLevel, Nodes: persisted.Nodes, byChunk: map[int64]int{}}
	active := map[int64]struct{}{}
	for _, row := range snapshot.rows {
		active[row.chunkID] = struct{}{}
	}
	for nodeIndex := range index.Nodes {
		node := &index.Nodes[nodeIndex]
		if len(node.Vector) != index.Dimension || node.Level+1 != len(node.Neighbors) {
			return nil, errors.New("nodo ANN corrupto")
		}
		for _, neighbors := range node.Neighbors {
			for _, neighbor := range neighbors {
				if neighbor < 0 || neighbor >= len(index.Nodes) {
					return nil, errors.New("arista ANN corrupta")
				}
			}
		}
		index.byChunk[node.ChunkID] = nodeIndex
		_, shouldBeActive := active[node.ChunkID]
		if shouldBeActive == node.Deleted {
			return nil, errANNStale
		}
		delete(active, node.ChunkID)
	}
	if len(active) != 0 || index.EntryPoint >= len(index.Nodes) {
		return nil, errANNStale
	}
	return index, nil
}

func saveHNSW(path string, signature indexSignature, index *hnswIndex) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	persisted := persistedHNSW{Magic: hnswFileMagic, Version: hnswFileVersion, Count: signature.count, Maximum: signature.maximum, Fingerprint: signature.fingerprint, Dimension: index.Dimension, M: index.M, EFConstruction: index.EFConstruction, EntryPoint: index.EntryPoint, MaxLevel: index.MaxLevel, Nodes: index.Nodes}
	if err := gob.NewEncoder(temporary).Encode(persisted); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func (index *hnswIndex) memoryBytes() int64 {
	if index == nil {
		return 0
	}
	var bytes int64
	for _, node := range index.Nodes {
		bytes += int64(len(node.Vector) * 4)
		for _, neighbors := range node.Neighbors {
			bytes += int64(len(neighbors) * 8)
		}
	}
	return bytes
}
