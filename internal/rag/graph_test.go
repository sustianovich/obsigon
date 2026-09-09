package rag

import (
	"context"
	"path/filepath"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

func TestPropagateDocumentScoresUsesDirectionDecayAndMaximumPath(t *testing.T) {
	edges := []db.GraphEdge{{SourceDocumentID: 1, TargetDocumentID: 2}, {SourceDocumentID: 2, TargetDocumentID: 3}, {SourceDocumentID: 4, TargetDocumentID: 1}}
	scores, hops := PropagateDocumentScores(map[int64]float64{1: 1}, edges, 2, .5, .7)
	if scores[2] != .5 || scores[3] != .25 || scores[4] != .35 || hops[3] != 2 {
		t.Fatalf("scores=%v hops=%v", scores, hops)
	}
	if _, present := scores[1]; present {
		t.Fatal("la semilla no debe reaparecer como candidata")
	}
}

func TestGraphRetrievalGuaranteesBestDirectNeighborAfterSemanticGate(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "graph.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	documents := []db.Document{
		{Path: "semilla.md", Title: "Semilla", SHA256: "1", Links: []db.Link{{Target: "Alcanzada"}}, Chunks: []db.Chunk{{Index: 0, Content: "evidencia principal", Embedding: []float32{1, 0}}}},
		{Path: "alcanzada.md", Title: "Alcanzada", SHA256: "2", Chunks: []db.Chunk{{Index: 0, Content: "fragmento inicial", Embedding: []float32{0, 1}}, {Index: 1, Content: "fragmento relacionado correcto", Embedding: []float32{.6, .8}}}},
	}
	for _, document := range documents {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	settings := config.Settings{
		EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto", TopK: 1,
		MinSimilarity: .85, MinRelativeScore: .62, HybridRRFK: 60, HybridSemanticWeight: 1,
		HybridCandidateMultiplier: 4, MaxChunksPerDocument: 2,
		GraphSearchEnabled: true, GraphWeight: .5, GraphMaxHops: 1, GraphDecay: .5,
		GraphBacklinkWeight: .7, GraphSeedDocuments: 1, GraphMaxCandidates: 20,
	}
	vectors := NewVectorStore(store, 2)
	retriever := Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: &fixedQueryEmbedder{vector: []float32{1, 0}}, Reranker: NewNoopReranker()}
	result, err := retriever.Retrieve(ctx, RetrievalRequest{Question: "evidencia principal", TopK: 1, MinSimilarity: .85, ExpandLinks: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Abstained || len(result.Hits) != 2 {
		t.Fatalf("resultado=%+v", result)
	}
	neighbor := result.Hits[1]
	if neighbor["path"] != "alcanzada.md" || neighbor["chunk_index"] != 1 || neighbor["reason"] != "búsqueda por grafo (enlace directo)" {
		t.Fatalf("vecino=%+v", neighbor)
	}

	blocked, err := retriever.Retrieve(ctx, RetrievalRequest{Question: "fuera de dominio", TopK: 1, MinSimilarity: 1.1, ExpandLinks: true})
	if err != nil || !blocked.Abstained || len(blocked.Hits) != 0 {
		t.Fatalf("el grafo anuló la abstención: result=%+v err=%v", blocked, err)
	}
}
