package rag

import (
	"math"
	"math/rand"
	"runtime"
	"sort"
	"testing"
	"time"
)

func BenchmarkVectorScan10K(b *testing.B)  { benchmarkVectorScan(b, 10_000, 384) }
func BenchmarkVectorScan50K(b *testing.B)  { benchmarkVectorScan(b, 50_000, 384) }
func BenchmarkVectorScan100K(b *testing.B) { benchmarkVectorScan(b, 100_000, 384) }

// BenchmarkSearchDocuments* mide el camino real de consulta (rankRows): filtros
// baratos primero, coseno sólo de los supervivientes y reparto por rangos
// contiguos entre workers. Reporta p50/p95 en microsegundos y el working set.
func BenchmarkSearchDocuments10K(b *testing.B)  { benchmarkSearchDocuments(b, 10_000, 384) }
func BenchmarkSearchDocuments50K(b *testing.B)  { benchmarkSearchDocuments(b, 50_000, 384) }
func BenchmarkSearchDocuments100K(b *testing.B) { benchmarkSearchDocuments(b, 100_000, 384) }

func BenchmarkANNSearch10K(b *testing.B) {
	const count, dimension = 10_000, 384
	random := rand.New(rand.NewSource(42))
	rows := make([]vectorRow, count)
	rowByChunk := make(map[int64]int, count)
	for rowIndex := range rows {
		vector := make([]float32, dimension)
		for offset := range vector {
			vector[offset] = random.Float32() - .5
		}
		normalizeVector(vector)
		rows[rowIndex] = vectorRow{chunkID: int64(rowIndex + 1), documentID: int64(rowIndex + 1), vector: vector}
		rowByChunk[int64(rowIndex+1)] = rowIndex
	}
	query := make([]float32, dimension)
	for offset := range query {
		query[offset] = random.Float32() - .5
	}
	normalizeVector(query)
	index := buildHNSW(rows, dimension, 16, 200)
	annMemory := float64(index.memoryBytes()) / (1024 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = rankANN(index, rows, rowByChunk, query, 20, .1, "", "", nil, nil, 64)
	}
	b.StopTimer()
	b.ReportMetric(annMemory, "ann-MiB")
	reportLatencyPercentiles(b, func() time.Duration {
		return timeCalls(8, func() { rankANN(index, rows, rowByChunk, query, 20, .1, "", "", nil, nil, 64) })
	})
}

func benchmarkSearchDocuments(b *testing.B, count, dimension int) {
	random := rand.New(rand.NewSource(42))
	rows := make([]vectorRow, count)
	for index := range rows {
		vector := make([]float32, dimension)
		for offset := range vector {
			vector[offset] = random.Float32() - .5
		}
		normalizeVector(vector)
		rows[index] = vectorRow{chunkID: int64(index + 1), documentID: int64(index/4 + 1), vector: vector}
	}
	query := make([]float32, dimension)
	for index := range query {
		query[index] = random.Float32() - .5
	}
	normalizeVector(query)
	store := &VectorStore{workers: runtime.GOMAXPROCS(0)}
	// Umbral ~2σ del coseno de vectores unitarios aleatorios de esta dimensión:
	// conserva una fracción pequeña, como una consulta real, sin evitar el
	// recorrido exhaustivo que fija la línea base.
	minSimilarity := 2.0 / math.Sqrt(float64(dimension))
	b.ReportAllocs()
	b.ReportMetric(float64(count*dimension*4)/(1024*1024), "working-set-MiB")
	b.ResetTimer()
	for range b.N {
		out := store.rankRows(rows, query, 20, minSimilarity, "", "", nil, nil)
		_ = out
	}
	b.StopTimer()
	reportLatencyPercentiles(b, func() time.Duration {
		return timeCalls(64, func() { store.rankRows(rows, query, 20, minSimilarity, "", "", nil, nil) })
	})
}

// timeCalls promedia un lote de llamadas para superar la resolución del reloj
// del sistema (en Windows, del orden de 1 ms), que hace inútil cronometrar una
// sola consulta rápida.
func timeCalls(batch int, call func()) time.Duration {
	start := time.Now()
	for range batch {
		call()
	}
	return time.Since(start) / time.Duration(batch)
}

func reportLatencyPercentiles(b *testing.B, sample func() time.Duration) {
	const samples = 40
	latencies := make([]time.Duration, samples)
	for index := range latencies {
		latencies[index] = sample()
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	at := func(quantile float64) float64 {
		index := int(quantile * float64(len(latencies)-1))
		return float64(latencies[index].Nanoseconds()) / 1000.0
	}
	b.ReportMetric(at(0.50), "p50-us")
	b.ReportMetric(at(0.95), "p95-us")
}

func benchmarkVectorScan(b *testing.B, count, dimension int) {
	random := rand.New(rand.NewSource(42))
	rows := make([][]float32, count)
	for index := range rows {
		rows[index] = make([]float32, dimension)
		for offset := range rows[index] {
			rows[index][offset] = random.Float32() - .5
		}
		normalizeVector(rows[index])
	}
	query := make([]float32, dimension)
	for index := range query {
		query[index] = random.Float32() - .5
	}
	normalizeVector(query)
	b.ReportAllocs()
	b.ReportMetric(float64(count*dimension*4)/(1024*1024), "working-set-MiB")
	b.ResetTimer()
	for range b.N {
		scores := make([]float64, count)
		for index, row := range rows {
			scores[index] = dot(row, query)
		}
		sort.Slice(scores, func(i, j int) bool { return scores[i] > scores[j] })
		if len(scores) == 0 {
			b.Fatal("sin puntuaciones")
		}
	}
}
