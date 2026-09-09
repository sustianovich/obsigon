package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"obsigon/internal/ollama"
)

type rewriteGenerator struct {
	content string
	err     error
	calls   int
	prompt  string
	options ollama.ChatOptions
}

func (g *rewriteGenerator) Chat(_ context.Context, instructions, prompt string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	g.calls++
	g.prompt, g.options = prompt, options
	if instructions != QueryRewriteInstructions {
		return ollama.GenerationResult{}, errors.New("instrucciones inesperadas")
	}
	if g.err != nil {
		return ollama.GenerationResult{}, g.err
	}
	return ollama.GenerationResult{Content: g.content, PromptTokens: 20, CompletionTokens: 7}, nil
}

func TestRewriteStandaloneQuestionResolvesFollowUpWithoutEmbeddingMemory(t *testing.T) {
	generator := &rewriteGenerator{content: `{"question":"¿Cuál es la dosis de amoxicilina?"}`}
	result := RewriteStandaloneQuestion(context.Background(), generator, "¿y su dosis?", "Turno anterior: hablamos de amoxicilina.", QueryRewriteOptions{Enabled: true, ContextTokens: 4096, MaxOutputTokens: 120, MaxMemoryChars: 2000})
	if result.Question != "¿Cuál es la dosis de amoxicilina?" || result.Metrics["status"] != "completed" || result.Metrics["changed"] != true {
		t.Fatalf("result=%+v", result)
	}
	if generator.calls != 1 || generator.options.MaxOutputTokens != 120 || !strings.Contains(generator.prompt, "amoxicilina") {
		t.Fatalf("generator=%+v", generator)
	}
	if got := BuildMemoryAwareRetrievalQuery("¿y su dosis?", "amoxicilina y mucho historial"); got != "¿y su dosis?" {
		t.Fatalf("la memoria volvió a entrar en el embedding: %q", got)
	}
}

func TestRewriteStandaloneQuestionFallsBackSafely(t *testing.T) {
	invalid := &rewriteGenerator{content: "La dosis es..."}
	result := RewriteStandaloneQuestion(context.Background(), invalid, "¿y su dosis?", "amoxicilina", QueryRewriteOptions{Enabled: true})
	if result.Question != "¿y su dosis?" || result.Metrics["status"] != "fallback_invalid_output" {
		t.Fatalf("invalid=%+v", result)
	}
	unused := &rewriteGenerator{content: `{"question":"no debe llamarse"}`}
	result = RewriteStandaloneQuestion(context.Background(), unused, "Pregunta autónoma", "", QueryRewriteOptions{Enabled: true})
	if unused.calls != 0 || result.Question != "Pregunta autónoma" || result.Metrics["status"] != "skipped_no_memory" {
		t.Fatalf("unused=%+v result=%+v", unused, result)
	}
}
