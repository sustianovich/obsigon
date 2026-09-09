package rag

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"obsigon/internal/ollama"
)

type agentTestGenerator struct {
	verifier      string
	writer        string
	verifierCalls int
	writerCalls   int
	streamCalls   int
	failVerifier  bool
}

func (g *agentTestGenerator) Chat(_ context.Context, instructions, _ string, _ ollama.ChatOptions) (ollama.GenerationResult, error) {
	if instructions == VerifierInstructions {
		g.verifierCalls++
		if g.failVerifier {
			return ollama.GenerationResult{}, errors.New("fallo simulado")
		}
		return ollama.GenerationResult{Content: g.verifier, PromptTokens: 10, CompletionTokens: 5}, nil
	}
	g.writerCalls++
	return ollama.GenerationResult{Content: g.writer, PromptTokens: 20, CompletionTokens: 8}, nil
}

func (g *agentTestGenerator) StreamChat(_ context.Context, _, _ string, _ ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	g.streamCalls++
	if err := onDelta(g.writer); err != nil {
		return ollama.GenerationResult{}, err
	}
	return ollama.GenerationResult{Content: g.writer, PromptTokens: 20, CompletionTokens: 8}, nil
}

func agentFixtureOptions() AgentOptions {
	return AgentOptions{
		VerificationEnabled: true, VerifierContextTokens: 8192,
		WriterContextTokens: 16384, VerifierMaxOutputTokens: 600,
		WriterMaxOutputTokens: 1800,
		ModelContext:          ModelContextInfo{Model: "qwen3.5:0.8b", ProfileID: "light", Window: 32768, Source: "profile", Verified: true},
	}
}

func agentFixtureHits() []map[string]any {
	return []map[string]any{{"title": "Nota", "path": "nota.md", "heading": "Sección", "reason": "búsqueda semántica", "content": "Contenido de prueba.", "metadata": map[string]any{}}}
}

func TestParseVerifierSufficiencyAndBody(t *testing.T) {
	report := "\n SUFICIENCIA: parcial \n\nEl resto del informe."
	if ParseVerifierSufficiency(report) != "parcial" || VerifierReportBody(report) != "El resto del informe." {
		t.Fatalf("informe=%q", report)
	}
	if ParseVerifierSufficiency("Informe sin formato") != "" {
		t.Fatal("un formato inválido no debe convertirse en insuficiente")
	}
}

func TestVerifierInsufficientCanSkipWriter(t *testing.T) {
	generator := &agentTestGenerator{verifier: "SUFICIENCIA: insuficiente\n\nFalta evidencia.", writer: "No debe ejecutarse"}
	options := agentFixtureOptions()
	options.AbstainOnInsufficient = true
	result, err := RunAgentPipeline(context.Background(), generator, "pregunta", agentFixtureHits(), options)
	if err != nil {
		t.Fatal(err)
	}
	writer := result.Metrics["writer"].(map[string]any)
	verifier := result.Metrics["verifier"].(map[string]any)
	if generator.verifierCalls != 1 || generator.writerCalls != 0 || writer["status"] != "skipped_insufficient_evidence" || verifier["sufficiency"] != "insuficiente" || result.Warning == "" {
		t.Fatalf("calls=%+v writer=%+v verifier=%+v warning=%q", generator, writer, verifier, result.Warning)
	}
}

func TestInvalidVerifierFormatDoesNotBlockWriter(t *testing.T) {
	generator := &agentTestGenerator{verifier: "Informe sin formato", writer: "Respuesta final [1]."}
	options := agentFixtureOptions()
	options.AbstainOnInsufficient = true
	result, err := RunAgentPipeline(context.Background(), generator, "pregunta", agentFixtureHits(), options)
	if err != nil {
		t.Fatal(err)
	}
	verifier := result.Metrics["verifier"].(map[string]any)
	if generator.writerCalls != 1 || result.Answer != "Respuesta final [1]." || verifier["sufficiency_parse_error"] != true {
		t.Fatalf("result=%+v verifier=%+v", result, verifier)
	}
}

func TestVerifierFailureDegradesToDirectWriter(t *testing.T) {
	generator := &agentTestGenerator{failVerifier: true, writer: "Respuesta final [1]."}
	result, err := RunAgentPipeline(context.Background(), generator, "pregunta", agentFixtureHits(), agentFixtureOptions())
	if err != nil {
		t.Fatal(err)
	}
	if generator.writerCalls != 1 || result.Warning == "" || result.Metrics["verifier"].(map[string]any)["status"] != "error" {
		t.Fatalf("result=%+v calls=%+v", result, generator)
	}
}

func TestAgentSyncAndStreamMetricsHaveRoleParity(t *testing.T) {
	options := agentFixtureOptions()
	syncGenerator := &agentTestGenerator{verifier: "SUFICIENCIA: suficiente\n\nInforme.", writer: "Respuesta final [1]."}
	syncResult, err := RunAgentPipeline(context.Background(), syncGenerator, "pregunta", agentFixtureHits(), options)
	if err != nil {
		t.Fatal(err)
	}
	streamGenerator := &agentTestGenerator{verifier: syncGenerator.verifier, writer: syncGenerator.writer}
	visible := ""
	streamResult, err := StreamAgentPipeline(context.Background(), streamGenerator, "pregunta", agentFixtureHits(), options, func(piece string) error { visible += piece; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if visible != syncResult.Answer || !reflect.DeepEqual(syncResult.Metrics["verifier"], streamResult.Metrics["verifier"]) || !reflect.DeepEqual(syncResult.Metrics["writer"], streamResult.Metrics["writer"]) {
		t.Fatalf("sync=%+v stream=%+v visible=%q", syncResult.Metrics, streamResult.Metrics, visible)
	}
	for _, role := range []string{"verifier", "writer"} {
		metrics := streamResult.Metrics[role].(map[string]any)
		if metrics["effective_context_window"].(int) <= 0 || metrics["remaining_tokens"].(int) != metrics["effective_context_window"].(int)-metrics["total_tokens"].(int) {
			t.Fatalf("rol=%s métricas=%+v", role, metrics)
		}
	}
}
