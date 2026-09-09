package rag

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"obsigon/internal/ollama"
)

type rerankFake struct {
	content string
	err     error
	calls   int
	options ollama.ChatOptions
}

func TestCrossEncoderRerankerUsesLocalRuntimeAndDegradesSafely(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNoContent)
		case "/rerank":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"scores":[{"id":0,"score":0.1},{"id":1,"score":0.95}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer runtime.Close()
	candidates := []map[string]any{{"chunk_id": int64(1), "fusion_score": .9, "content": "general"}, {"chunk_id": int64(2), "fusion_score": .7, "content": "exacto"}}
	reranker := NewCrossEncoderReranker(runtime.URL, "modelo-local", true, 2, .8, time.Second)
	if availability := reranker.Availability(context.Background()); !availability.Available || availability.Backend != "cross_encoder" {
		t.Fatalf("availability=%+v", availability)
	}
	got, outcome := reranker.Rerank(context.Background(), "pregunta", candidates)
	if mustInt64(got[0]["chunk_id"]) != 2 || outcome.Status != "completed" || outcome.Scored != 2 {
		t.Fatalf("got=%+v outcome=%+v", got, outcome)
	}

	broken := NewCrossEncoderReranker("http://127.0.0.1:1", "modelo", true, 2, .8, 50*time.Millisecond)
	degraded, degradedOutcome := broken.Rerank(context.Background(), "pregunta", candidates)
	if !reflect.DeepEqual(degraded, candidates) || degradedOutcome.Status != "degraded" {
		t.Fatalf("degraded=%+v outcome=%+v", degraded, degradedOutcome)
	}
}

func (f *rerankFake) Chat(_ context.Context, _, _ string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	f.calls++
	f.options = options
	if f.err != nil {
		return ollama.GenerationResult{}, f.err
	}
	return ollama.GenerationResult{Content: f.content}, nil
}

func (f *rerankFake) Status(context.Context) map[string]any {
	return map[string]any{"accessible": f.err == nil}
}

func TestLLMRerankerImprovesOrderAndMixesFusionScore(t *testing.T) {
	fake := &rerankFake{content: `{"scores":[{"id":0,"score":0.0},{"id":1,"score":1.0}]}`}
	reranker := NewLLMReranker(fake, true, 2, .8)
	candidates := []map[string]any{
		{"chunk_id": int64(1), "fusion_score": .9, "content": "menos relevante"},
		{"chunk_id": int64(2), "fusion_score": .7, "content": "respuesta exacta"},
	}
	got, outcome := reranker.Rerank(context.Background(), "pregunta", candidates)
	if mustInt64(got[0]["chunk_id"]) != 2 || outcome.Status != "completed" || !outcome.Available || outcome.Moved != 2 {
		t.Fatalf("resultado=%+v outcome=%+v", got, outcome)
	}
	if fake.options.Temperature != 0 || fake.options.Think != false {
		t.Fatalf("opciones no deterministas: %+v", fake.options)
	}
}

func TestLLMRerankerFailureKeepsPreviousOrder(t *testing.T) {
	for _, fake := range []*rerankFake{{err: errors.New("apagado")}, {content: "formato inesperado"}} {
		candidates := []map[string]any{{"chunk_id": int64(1), "fusion_score": .8}, {"chunk_id": int64(2), "fusion_score": .7}}
		got, outcome := NewLLMReranker(fake, true, 2, .5).Rerank(context.Background(), "pregunta", candidates)
		if !reflect.DeepEqual(got, candidates) || outcome.Status != "degraded" {
			t.Fatalf("got=%+v outcome=%+v", got, outcome)
		}
	}
}

func TestLLMRerankerNeverChangesHardAbstention(t *testing.T) {
	store, vectors, settings := retrievalFixture(t)
	settings.RerankEnabled = true
	fake := &rerankFake{content: `{"scores":[]}`}
	retriever := Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: &fixedQueryEmbedder{vector: []float32{1, 0, 0}}, Reranker: NewLLMReranker(fake, true, 10, .5)}
	result, err := retriever.Retrieve(context.Background(), RetrievalRequest{Question: "texto", MinSimilarity: 1.1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Abstained || len(result.Hits) != 0 || fake.calls != 0 {
		t.Fatalf("resultado=%+v llamadas=%d", result, fake.calls)
	}
}
