package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

type evaluationEmbedder struct{ vector []float32 }

func (e evaluationEmbedder) Dimension(context.Context) (int, error) { return len(e.vector), nil }
func (e evaluationEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index := range texts {
		result[index] = append([]float32(nil), e.vector...)
	}
	return result, nil
}

type evaluationGenerator struct{ calls int }

func (g *evaluationGenerator) Chat(_ context.Context, instructions, _ string, _ ollama.ChatOptions) (ollama.GenerationResult, error) {
	g.calls++
	if instructions == rag.QueryRewriteInstructions {
		return ollama.GenerationResult{Content: `{"question":"¿Qué exige el procedimiento de control y trazabilidad?"}`}, nil
	}
	if instructions == rag.VerifierInstructions {
		return ollama.GenerationResult{Content: "SUFICIENCIA: suficiente\n\nLa fuente permite responder [1]."}, nil
	}
	return ollama.GenerationResult{Content: "El procedimiento exige control y trazabilidad [1]."}, nil
}

func TestEvaluateMeasuresAutonomousFollowUpRewrite(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "rewrite-evaluation.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ReplaceDocument(ctx, db.Document{
		Path: "procedimiento.md", Title: "Procedimiento", SHA256: "one", Metadata: map[string]any{},
		Chunks: []db.Chunk{{Index: 0, Content: "El procedimiento exige control y trazabilidad.", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		ChatModel: "qwen-local", EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		TopK: 1, MinSimilarity: .1, MinRelativeScore: .1, HybridCandidateMultiplier: 4,
		HybridRRFK: 60, HybridSemanticWeight: 1, MaxChunksPerDocument: 2,
		QueryRewriteEnabled: true, QueryRewriteContextTokens: 4096, QueryRewriteMaxOutputTokens: 120, QueryRewriteMaxMemoryChars: 4000,
		VerifierContextTokens: 4096, WriterContextTokens: 4096, VerifierMaxOutputTokens: 256, MaxOutputTokens: 256,
	}
	generator := &evaluationGenerator{}
	report, err := evaluate(ctx, []evaluationCase{{
		ID: "follow-up", Question: "¿y sus exigencias?", Memory: "El turno anterior trata del procedimiento de control y trazabilidad.",
		ExpectedRewriteTerms: []string{"procedimiento", "control", "trazabilidad"}, ExpectedPaths: []string{"procedimiento.md"},
		ExpectedTerms: [][]string{{"control"}, {"trazabilidad"}},
	}}, settings, store, rag.NewVectorStore(store, 1), evaluationEmbedder{vector: []float32{1, 0}}, generator)
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if generator.calls != 3 || result.QueryRewritePass == nil || !*result.QueryRewritePass || result.QueryRewriteStatus != "completed" || !result.Pass {
		t.Fatalf("llamadas=%d result=%+v", generator.calls, result)
	}
	if report.Metrics["recall_at_k"] != float64(1) || report.Metrics["query_rewrite_pass_rate"] != float64(1) {
		t.Fatalf("metrics=%+v", report.Metrics)
	}
}

func (g *evaluationGenerator) StreamChat(ctx context.Context, instructions, prompt string, options ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	result, err := g.Chat(ctx, instructions, prompt, options)
	if err == nil {
		err = onDelta(result.Content)
	}
	return result, err
}

func TestScoreCaseAndSummaryMatchEvaluationDefinitions(t *testing.T) {
	answerable := scoreCase(evaluationCase{
		ID:             "answerable",
		ExpectedPaths:  []string{"notes/a.md", "notes/b.md"},
		ForbiddenPaths: []string{"notes/forbidden.md"},
	}, []string{"notes/a.md", "notes/a.md", "notes/b.md"}, false)
	if !answerable.Pass {
		t.Fatal("debia aceptar recall completo sin rutas prohibidas")
	}
	if got := deref(answerable.DocumentRecall); got != 1 {
		t.Fatalf("recall=%v, esperado 1", got)
	}
	if got := deref(answerable.PrecisionAtK); math.Abs(got-1) > 1e-12 {
		t.Fatalf("precision=%v, esperado 1", got)
	}

	abstention := scoreCase(evaluationCase{ID: "negative", ExpectAbstention: true}, nil, true)
	metrics := summarize([]caseResult{answerable, abstention})
	if got := metrics["mean_reciprocal_rank"].(*float64); *got != 1 {
		t.Fatalf("mrr=%v, esperado 1", *got)
	}
	if got := metrics["abstention_recall"].(float64); got != 1 {
		t.Fatalf("recall de abstencion=%v, esperado 1", got)
	}
}

func TestScoreCaseRejectsForbiddenPath(t *testing.T) {
	result := scoreCase(evaluationCase{
		ExpectedPaths:  []string{"notes/a.md"},
		ForbiddenPaths: []string{"notes/old.md"},
	}, []string{"notes/a.md", "notes/old.md"}, false)
	if result.Pass {
		t.Fatal("una ruta prohibida debe hacer fallar el caso")
	}
}

func TestNDCGDeduplicatesDocuments(t *testing.T) {
	got := ndcgAtK([]string{"a", "a", "b"}, map[string]bool{"a": true, "b": true})
	if math.Abs(got-1) > 1e-12 {
		t.Fatalf("nDCG=%v, esperado 1", got)
	}
}

func TestScoreAnswerRequiresExpectedTermsAndValidCitations(t *testing.T) {
	item := evaluationCase{ExpectedTerms: [][]string{{"control"}, {"trazabilidad", "auditoría"}}}
	passing := caseResult{RetrievalPass: true, Pass: true}
	scoreAnswer(&passing, item, "Incluye control y trazabilidad [1].", 1)
	if !passing.Pass || passing.AnswerPass == nil || !*passing.AnswerPass || deref(passing.AnswerTermRecall) != 1 {
		t.Fatalf("respuesta válida=%+v", passing)
	}

	failing := caseResult{RetrievalPass: true, Pass: true}
	scoreAnswer(&failing, item, "Incluye control sin explicar el resto [2].", 1)
	if failing.Pass || failing.AnswerPass == nil || *failing.AnswerPass || failing.Citations == nil || !failing.Citations.HasInvalid {
		t.Fatalf("respuesta inválida=%+v", failing)
	}
}

func TestEvaluateRunsAgentPipelineAndReportsEndToEndQuality(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "evaluation.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ReplaceDocument(ctx, db.Document{
		Path: "procedimiento.md", Title: "Procedimiento", SHA256: "one", Metadata: map[string]any{},
		Chunks: []db.Chunk{{Index: 0, Content: "El procedimiento exige control y trazabilidad.", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		ChatModel: "qwen-local", EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		TopK: 1, MinSimilarity: .1, MinRelativeScore: .1, HybridCandidateMultiplier: 4,
		HybridRRFK: 60, HybridSemanticWeight: 1, MaxChunksPerDocument: 2,
		VerifierContextTokens: 4096, WriterContextTokens: 4096, VerifierMaxOutputTokens: 256, MaxOutputTokens: 256,
	}
	embedder := evaluationEmbedder{vector: []float32{1, 0}}
	generator := &evaluationGenerator{}
	report, err := evaluate(ctx, []evaluationCase{{
		ID: "quality", Question: "¿Qué exige?", ExpectedPaths: []string{"procedimiento.md"},
		ExpectedTerms: [][]string{{"control"}, {"trazabilidad"}},
	}}, settings, store, rag.NewVectorStore(store, 1), embedder, generator)
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 2 || report.Passes != 1 || len(report.Results) != 1 || !report.Results[0].Pass {
		t.Fatalf("llamadas=%d informe=%+v", generator.calls, report)
	}
	if report.Metrics["end_to_end_pass_rate"] != float64(1) || report.Metrics["citation_valid_rate"] != float64(1) {
		t.Fatalf("métricas=%+v", report.Metrics)
	}
}
