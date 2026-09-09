package rag

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
)

type fixedQueryEmbedder struct {
	vector []float32
	calls  int
}

func TestVectorReloadDoesNotBlockConcurrentSearch(t *testing.T) {
	store, vectors, _ := retrievalFixture(t)
	ctx := context.Background()
	old, err := vectors.Search(ctx, []float32{1, 0, 0}, 10, .5, "", "", nil)
	if err != nil || len(old) == 0 {
		t.Fatalf("carga inicial: %v %+v", err, old)
	}
	if err := store.ReplaceDocument(ctx, db.Document{Path: "tres.md", Title: "Nuevo", SHA256: "3", Metadata: map[string]any{}, Chunks: []db.Chunk{{Index: 0, Content: "nuevo", Embedding: []float32{1, 0, 0}}}}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	vectors.beforeLoad = func() { close(started); <-release }
	reloaded := make(chan error, 1)
	go func() {
		_, searchErr := vectors.Search(ctx, []float32{1, 0, 0}, 10, .5, "", "", nil)
		reloaded <- searchErr
	}()
	<-started
	begin := time.Now()
	concurrent, err := vectors.Search(ctx, []float32{1, 0, 0}, 10, .5, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("la búsqueda se bloqueó durante la recarga: %v", elapsed)
	}
	if len(concurrent) != len(old) {
		t.Fatalf("snapshot parcial: antiguo=%d concurrente=%d", len(old), len(concurrent))
	}
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	vectors.beforeLoad = nil
	updated, err := vectors.Search(ctx, []float32{1, 0, 0}, 10, .5, "", "", nil)
	if err != nil || len(updated) <= len(old) {
		t.Fatalf("snapshot nuevo no publicado: err=%v resultados=%d", err, len(updated))
	}
}

func (f *fixedQueryEmbedder) Dimension(context.Context) (int, error) { return len(f.vector), nil }
func (f *fixedQueryEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	result := make([][]float32, len(texts))
	for index := range texts {
		result[index] = append([]float32(nil), f.vector...)
	}
	return result, nil
}

func retrievalFixture(t *testing.T) (*db.Store, *VectorStore, config.Settings) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "retrieval.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	documents := []db.Document{
		{Path: "uno.md", Title: "Protocolo validación", SHA256: "1", Metadata: map[string]any{"estado": "aprobado", "tipo": "protocolo", "aliases": []string{"PV", "Validación"}, "_tags": []string{"normativa", "protocolo", "proyecto/clinica"}, "_vigencia": map[string]any{"estado_temporal": "vigente"}}, Chunks: []db.Chunk{{Index: 0, Heading: "Criterios", Content: "La validación exige dos comprobaciones críticas.", Embedding: []float32{1, 0, 0}}, {Index: 1, Heading: "Detalle", Content: "Comprobación adicional del protocolo.", Embedding: []float32{.9, .1, 0}}}},
		{Path: "dos.md", Title: "Borrador alternativo", SHA256: "2", Metadata: map[string]any{"estado": "borrador", "_tags": []string{"normativa"}, "_vigencia": map[string]any{"estado_temporal": "caducada"}}, Chunks: []db.Chunk{{Index: 0, Heading: "Otro", Content: "Texto alternativo sin evidencia suficiente.", Embedding: []float32{0, 1, 0}}}},
	}
	for _, document := range documents {
		if err := store.ReplaceDocument(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	settings := config.Settings{EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto", TopK: 3, MinSimilarity: .3, MinRelativeScore: .62, HybridSearchEnabled: true, HybridRRFK: 60, HybridSemanticWeight: 1, HybridLexicalWeight: .8, HybridCandidateMultiplier: 4, MMREnabled: true, MMRLambda: .72, MaxChunksPerDocument: 1}
	return store, NewVectorStore(store, 3), settings
}

func TestVectorStoreCosineAndANDFilters(t *testing.T) {
	store, vectors, _ := retrievalFixture(t)
	_ = store
	ctx := context.Background()
	results, err := vectors.Search(ctx, []float32{1, 0, 0}, 10, .5, "aprobado", "no_caducada", []string{"normativa", "protocolo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Score < .99999 {
		t.Fatalf("resultados=%+v", results)
	}
	empty, err := vectors.Search(ctx, []float32{1, 0}, 10, -1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatal("se aceptó una dimensión incompatible")
	}
	zero, err := vectors.Search(ctx, []float32{0, 0, 0}, 10, -1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero) != 0 {
		t.Fatal("el vector nulo produjo resultados")
	}
}

func TestRetrievalHybridAndHardSemanticAbstention(t *testing.T) {
	store, vectors, settings := retrievalFixture(t)
	embedder := &fixedQueryEmbedder{vector: []float32{1, 0, 0}}
	retriever := Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: embedder, Reranker: NewNoopReranker()}
	result, err := retriever.Retrieve(context.Background(), RetrievalRequest{Question: "validacion comprobaciones", TopK: 2, MinSimilarity: .3, Status: "aprobado", Vigencia: "no_caducada", Tags: []string{"normativa", "protocolo"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Abstained || len(result.Hits) == 0 {
		t.Fatalf("resultado=%+v", result)
	}
	if result.Hits[0]["semantic_score"] == nil || result.Hits[0]["lexical_score"] == nil {
		t.Fatalf("no fusionó ambas ramas: %+v", result.Hits[0])
	}
	if result.Rerank.Available || result.Rerank.Backend != "noop" {
		t.Fatalf("reranker deshonesto: %+v", result.Rerank)
	}
	abstained, err := retriever.Retrieve(context.Background(), RetrievalRequest{Question: "validacion comprobaciones", TopK: 2, MinSimilarity: 1.1})
	if err != nil {
		t.Fatal(err)
	}
	if !abstained.Abstained || len(abstained.Hits) != 0 {
		t.Fatalf("FTS anuló la abstención: %+v", abstained)
	}
}

func TestRetrievalSupportsNestedTagsAndArbitraryFrontmatter(t *testing.T) {
	store, vectors, settings := retrievalFixture(t)
	retriever := Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: &fixedQueryEmbedder{vector: []float32{1, 0, 0}}, Reranker: NewNoopReranker()}
	for _, frontmatter := range []map[string]any{{"tipo": "protocolo"}, {"aliases": "PV"}} {
		result, err := retriever.Retrieve(context.Background(), RetrievalRequest{Question: "validación", TopK: 2, MinSimilarity: .3, Tags: []string{"proyecto"}, Frontmatter: frontmatter})
		if err != nil || result.Abstained || len(result.Hits) == 0 || result.Hits[0]["path"] != "uno.md" {
			t.Fatalf("frontmatter=%v result=%+v err=%v", frontmatter, result, err)
		}
	}
	empty, err := retriever.Retrieve(context.Background(), RetrievalRequest{Question: "validación", TopK: 2, MinSimilarity: .3, Frontmatter: map[string]any{"tipo": "decisión"}})
	if err != nil || !empty.Abstained || len(empty.Hits) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
}

func TestRelativeCutoffNeverEmptiesBest(t *testing.T) {
	candidates := []SemanticCandidate{{Score: .05}, {Score: .01}}
	for _, ratio := range []float64{0, .5, .99} {
		kept := ApplyRelativeCutoff(candidates, ratio)
		if len(kept) == 0 || kept[0].Score != .05 {
			t.Fatalf("ratio=%v resultado=%+v", ratio, kept)
		}
	}
}

func TestHubBoostPromotesBacklinkHubsAndIsAuditable(t *testing.T) {
	candidates := []map[string]any{
		{"document_id": int64(1), "fusion_score": .50},
		{"document_id": int64(2), "fusion_score": .46},
	}
	ApplyHubBoost(candidates, map[int64]int{1: 0, 2: 10}, .08)
	if candidates[0]["document_id"] != int64(2) || candidates[0]["backlink_count"] != 10 || math.Abs(floatValue(candidates[0]["hub_boost"])-.08) > 1e-12 || candidates[1]["fusion_score_before_hub"] != .50 {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestLexicalUtilitiesFollowRetrievalPolicy(t *testing.T) {
	query, terms := BuildFTSMatchQuery("¿Cuáles son las fases del alta?")
	if slices.Contains(terms, "cuales") || !slices.Contains(terms, "fases") || !slices.Contains(terms, "alta") || !strings.HasPrefix(query, `"`) {
		t.Fatalf("query=%q terms=%v", query, terms)
	}
	coverage, matched := LexicalTermCoverage(map[string]any{"title": "Política de validación", "heading": "Criterios", "content": "La validacion exige dos comprobaciones.", "path": "ref/politica.md"}, []string{"validación", "comprobaciones"})
	if matched != 2 || coverage != 1 {
		t.Fatalf("coverage=%v matched=%d", coverage, matched)
	}
	if RequiredLexicalMatches(1) != 1 || RequiredLexicalMatches(5) != 2 || RequiredLexicalMatches(30) != 6 {
		t.Fatal("umbral léxico incorrecto")
	}
}

func TestCapPerDocumentYieldsWhenNoAlternative(t *testing.T) {
	candidates := []map[string]any{}
	for index := 0; index < 4; index++ {
		candidates = append(candidates, map[string]any{"document_id": int64(1), "chunk_id": int64(index)})
	}
	if got := capPerDocument(candidates, 3, 1); len(got) != 3 {
		t.Fatalf("seleccionados=%d", len(got))
	}
	mixed := append([]map[string]any{}, candidates...)
	mixed = append(mixed, map[string]any{"document_id": int64(2), "chunk_id": int64(99)})
	got := capPerDocument(mixed, 3, 2)
	documents := []int64{mustInt64(got[0]["document_id"]), mustInt64(got[1]["document_id"]), mustInt64(got[2]["document_id"])}
	if documents[0] != 1 || documents[1] != 1 || documents[2] != 2 {
		t.Fatalf("documentos=%v", documents)
	}
}
