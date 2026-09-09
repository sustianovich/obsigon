package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

// scriptedClient es un evalClient determinista: sirve como "Ollama real" al
// grabar y permite comparar la reproducción sin red.
type scriptedClient struct {
	vector []float32
	calls  int
}

func (s *scriptedClient) Dimension(context.Context) (int, error) { return len(s.vector), nil }

func (s *scriptedClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for index := range texts {
		out[index] = append([]float32(nil), s.vector...)
	}
	return out, nil
}

func (s *scriptedClient) Chat(_ context.Context, instructions, _ string, _ ollama.ChatOptions) (ollama.GenerationResult, error) {
	s.calls++
	switch instructions {
	case rag.QueryRewriteInstructions:
		return ollama.GenerationResult{Content: `{"question":"¿Qué exige el procedimiento de control y trazabilidad?"}`}, nil
	case rag.VerifierInstructions:
		return ollama.GenerationResult{Content: "SUFICIENCIA: suficiente\n\nLa fuente permite responder [1]."}, nil
	default:
		return ollama.GenerationResult{Content: "El procedimiento exige control y trazabilidad [1]."}, nil
	}
}

func (s *scriptedClient) StreamChat(ctx context.Context, instructions, prompt string, options ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	result, err := s.Chat(ctx, instructions, prompt, options)
	if err == nil && onDelta != nil {
		err = onDelta(result.Content)
	}
	return result, err
}

func (s *scriptedClient) ModelShow(context.Context, string) (map[string]any, error) {
	return map[string]any{"model_info": map[string]any{}}, nil
}

func evaluationFixtureCase() evaluationCase {
	return evaluationCase{
		ID: "quality", Question: "¿Qué exige?", ExpectedPaths: []string{"procedimiento.md"},
		ExpectedTerms: [][]string{{"control"}, {"trazabilidad"}},
	}
}

func fixtureSettings() config.Settings {
	return config.Settings{
		ChatModel: "qwen-local", EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		TopK: 1, MinSimilarity: .1, MinRelativeScore: .1, HybridCandidateMultiplier: 4,
		HybridRRFK: 60, HybridSemanticWeight: 1, MaxChunksPerDocument: 2,
		VerifierContextTokens: 4096, WriterContextTokens: 4096, VerifierMaxOutputTokens: 256, MaxOutputTokens: 256,
	}
}

func seedFixtureStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "fixture-eval.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ReplaceDocument(context.Background(), db.Document{
		Path: "procedimiento.md", Title: "Procedimiento", SHA256: "one", Metadata: map[string]any{},
		Chunks: []db.Chunk{{Index: 0, Content: "El procedimiento exige control y trazabilidad.", Embedding: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestFixtureRoundTripReproducesReport graba las respuestas de un cliente
// determinista y comprueba que reproducir desde el fixture, sin volver a tocar
// el "Ollama", produce un informe idéntico.
func TestFixtureRoundTripReproducesReport(t *testing.T) {
	ctx := context.Background()
	settings := fixtureSettings()
	cases := []evaluationCase{evaluationFixtureCase()}

	recorder := &recordingClient{inner: &scriptedClient{vector: []float32{1, 0}}, data: newFixtureData("fixture-test")}
	recordedReport, err := evaluate(ctx, cases, settings, seedFixtureStore(t), rag.NewVectorStore(seedFixtureStore(t), 1), recorder, recorder)
	if err != nil {
		t.Fatalf("grabación: %v", err)
	}

	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := recorder.data.save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadFixtureData(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Embed) == 0 || len(loaded.Chat) == 0 {
		t.Fatalf("fixture vacío: embed=%d chat=%d", len(loaded.Embed), len(loaded.Chat))
	}

	replay := &replayClient{data: loaded}
	replayStore := seedFixtureStore(t)
	replayReport, err := evaluate(ctx, cases, settings, replayStore, rag.NewVectorStore(replayStore, 1), replay, replay)
	if err != nil {
		t.Fatalf("reproducción: %v", err)
	}
	if !reflect.DeepEqual(recordedReport.Metrics, replayReport.Metrics) {
		t.Fatalf("métricas divergen\n grabado=%+v\n replay=%+v", recordedReport.Metrics, replayReport.Metrics)
	}
	if !reflect.DeepEqual(recordedReport.Results, replayReport.Results) {
		t.Fatalf("resultados divergen\n grabado=%+v\n replay=%+v", recordedReport.Results, replayReport.Results)
	}
}

func TestReplayClientFailsOnMissingKey(t *testing.T) {
	replay := &replayClient{data: newFixtureData("empty")}
	if _, err := replay.Embed(context.Background(), []string{"desconocido"}); err == nil || !strings.Contains(err.Error(), "-record") {
		t.Fatalf("esperaba error de fixture incompleto, got %v", err)
	}
	if _, err := replay.Chat(context.Background(), "sys", "user", ollama.ChatOptions{}); err == nil {
		t.Fatal("Chat sin grabar debe fallar")
	}
}

func TestRecordingClientCachesAndReplaysModelErrors(t *testing.T) {
	client := &failingChatClient{scriptedClient: scriptedClient{vector: []float32{1, 0}}}
	recorder := &recordingClient{inner: client, data: newFixtureData("errors")}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := recorder.Chat(context.Background(), "instructions", "prompt", ollama.ChatOptions{}); err == nil || err.Error() != "fallo grabado" {
			t.Fatalf("intento %d: error inesperado %v", attempt, err)
		}
	}
	if client.failedCalls != 1 {
		t.Fatalf("el error no se deduplicó: llamadas=%d", client.failedCalls)
	}
	replay := &replayClient{data: recorder.data}
	if _, err := replay.Chat(context.Background(), "instructions", "prompt", ollama.ChatOptions{}); err == nil || err.Error() != "fallo grabado" {
		t.Fatalf("el replay no conservó el error: %v", err)
	}
}

type failingChatClient struct {
	scriptedClient
	failedCalls int
}

func (c *failingChatClient) Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error) {
	c.failedCalls++
	return ollama.GenerationResult{}, errors.New("fallo grabado")
}

func TestCheckThresholds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thresholds.json")
	metrics := map[string]float64{}
	for _, name := range requiredQualityMetrics {
		metrics[name] = 0.5
	}
	metrics["document_recall"] = 0.8
	metrics["ndcg_at_k"] = 0.7
	metrics["missing_metric"] = 0.5
	spec := thresholdSpec{Metrics: metrics}
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	observed := map[string]any{}
	for _, name := range requiredQualityMetrics {
		observed[name] = 1.0
	}
	observed["document_recall"] = 0.91
	observed["ndcg_at_k"] = 0.62
	summary, ok, err := checkThresholds(observed, path)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ndcg 0.62 < 0.70 y missing_metric ausente: no debería pasar")
	}
	if !strings.Contains(summary, "POR DEBAJO") || !strings.Contains(summary, "SIN VALOR") {
		t.Fatalf("resumen incompleto:\n%s", summary)
	}

	observed["document_recall"] = 0.81
	observed["ndcg_at_k"] = 0.71
	observed["missing_metric"] = 0.5
	_, ok, err = checkThresholds(observed, path)
	if err != nil || !ok {
		t.Fatalf("debería pasar: ok=%v err=%v", ok, err)
	}
}

func TestCheckThresholdsRejectsMissingRequiredMetric(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thresholds.json")
	raw, _ := json.Marshal(thresholdSpec{Metrics: map[string]float64{"document_recall": 0.8}})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkThresholds(map[string]any{"document_recall": 1.0}, path); err == nil || !strings.Contains(err.Error(), "obligatoria") {
		t.Fatalf("se esperaba rechazo por métrica obligatoria ausente, got %v", err)
	}
}
