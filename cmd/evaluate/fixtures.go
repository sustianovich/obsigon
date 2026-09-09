package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"obsigon/internal/ollama"
)

// evalClient es la superficie que cmd/evaluate necesita de Ollama. La
// implementa el cliente real, el grabador y el reproductor determinista.
type evalClient interface {
	Embed(context.Context, []string) ([][]float32, error)
	Dimension(context.Context) (int, error)
	Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error)
	StreamChat(context.Context, string, string, ollama.ChatOptions, func(string) error) (ollama.GenerationResult, error)
	ModelShow(context.Context, string) (map[string]any, error)
}

// fixtureData es el registro serializable de todas las respuestas de Ollama que
// consume una ejecución del evaluador: embeddings de indexación y consulta,
// generaciones del reformulador, verificador, redactor y reranker, y los
// metadatos de modelo. Con él, `-fixtures` reproduce la evaluación extremo a
// extremo sin red y de forma determinista.
type fixtureData struct {
	FormatVersion int                        `json:"format_version"`
	Dataset       string                     `json:"dataset"`
	Metadata      fixtureMetadata            `json:"metadata"`
	Dimension     int                        `json:"dimension"`
	Embed         map[string][][]float32     `json:"embed"`
	Chat          map[string]chatFixture     `json:"chat"`
	Model         map[string]json.RawMessage `json:"model"`
}

type chatFixture struct {
	Result  ollama.GenerationResult `json:"result"`
	Content string                  `json:"content"`
	Error   string                  `json:"error,omitempty"`
}

func newFixtureData(dataset string) *fixtureData {
	return &fixtureData{
		Dataset: dataset,
		Embed:   map[string][][]float32{},
		Chat:    map[string]chatFixture{},
		Model:   map[string]json.RawMessage{},
	}
}

func loadFixtureData(path string) (*fixtureData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data := newFixtureData("")
	if err := json.Unmarshal(raw, data); err != nil {
		return nil, fmt.Errorf("fixture %s ilegible: %w", path, err)
	}
	return data, nil
}

func (f *fixtureData) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func embedKey(texts []string) string {
	sum := sha256.Sum256([]byte("embed\x00" + strings.Join(texts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func chatKey(prefix, instructions, prompt string, options ollama.ChatOptions) string {
	signature := fmt.Sprintf("%s\x00%s\x00%s\x00mo=%d;t=%g;cw=%d;think=%v",
		prefix, instructions, prompt, options.MaxOutputTokens, options.Temperature, options.ContextWindow, options.Think)
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:])
}

// recordingClient delega cada llamada en el cliente real y guarda la respuesta
// en un fixtureData. Las escrituras se serializan porque la indexación puede
// pedir embeddings en paralelo.
type recordingClient struct {
	inner evalClient
	mu    sync.Mutex
	data  *fixtureData
}

func (r *recordingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	key := embedKey(texts)
	r.mu.Lock()
	cached, ok := r.data.Embed[key]
	r.mu.Unlock()
	if ok {
		return cached, nil
	}
	vectors, err := r.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.data.Embed[key] = vectors
	r.mu.Unlock()
	return vectors, nil
}

func (r *recordingClient) Dimension(ctx context.Context) (int, error) {
	r.mu.Lock()
	cached := r.data.Dimension
	r.mu.Unlock()
	if cached > 0 {
		return cached, nil
	}
	dimension, err := r.inner.Dimension(ctx)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.data.Dimension = dimension
	r.mu.Unlock()
	return dimension, nil
}

func (r *recordingClient) Chat(ctx context.Context, instructions, prompt string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	key := chatKey("chat", instructions, prompt, options)
	r.mu.Lock()
	cached, ok := r.data.Chat[key]
	r.mu.Unlock()
	if ok {
		if cached.Error != "" {
			return cached.Result, errors.New(cached.Error)
		}
		return cached.Result, nil
	}
	result, err := r.inner.Chat(ctx, instructions, prompt, options)
	r.mu.Lock()
	stored := chatFixture{Result: result, Content: result.Content}
	if err != nil {
		stored.Error = err.Error()
	}
	r.data.Chat[key] = stored
	r.mu.Unlock()
	return result, err
}

func (r *recordingClient) StreamChat(ctx context.Context, instructions, prompt string, options ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	key := chatKey("streamchat", instructions, prompt, options)
	r.mu.Lock()
	cached, ok := r.data.Chat[key]
	r.mu.Unlock()
	if ok {
		if cached.Error != "" {
			return cached.Result, errors.New(cached.Error)
		}
		if onDelta != nil && cached.Content != "" {
			if err := onDelta(cached.Content); err != nil {
				return cached.Result, err
			}
		}
		return cached.Result, nil
	}
	result, err := r.inner.StreamChat(ctx, instructions, prompt, options, onDelta)
	r.mu.Lock()
	stored := chatFixture{Result: result, Content: result.Content}
	if err != nil {
		stored.Error = err.Error()
	}
	r.data.Chat[key] = stored
	r.mu.Unlock()
	return result, err
}

func (r *recordingClient) ModelShow(ctx context.Context, model string) (map[string]any, error) {
	key := "modelshow\x00" + model
	r.mu.Lock()
	cached, ok := r.data.Model[key]
	r.mu.Unlock()
	if ok {
		payload := map[string]any{}
		if err := json.Unmarshal(cached, &payload); err == nil {
			return payload, nil
		}
	}
	payload, err := r.inner.ModelShow(ctx, model)
	stored := payload
	if err != nil || stored == nil {
		stored = map[string]any{}
	}
	encoded, marshalErr := json.Marshal(stored)
	if marshalErr == nil {
		r.mu.Lock()
		r.data.Model[key] = encoded
		r.mu.Unlock()
	}
	return payload, err
}

// replayClient sirve exclusivamente respuestas grabadas. Una clave ausente es un
// error fatal: significa que el fixture está desfasado y hay que regenerarlo con
// `-record`.
type replayClient struct{ data *fixtureData }

func (r *replayClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors, ok := r.data.Embed[embedKey(texts)]
	if !ok {
		return nil, missingFixture("embed", fmt.Sprintf("%d textos, primero=%q", len(texts), first(texts)))
	}
	return vectors, nil
}

func (r *replayClient) Dimension(context.Context) (int, error) {
	if r.data.Dimension == 0 {
		return 0, missingFixture("dimension", "sin valor grabado")
	}
	return r.data.Dimension, nil
}

func (r *replayClient) Chat(_ context.Context, instructions, prompt string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	fixture, ok := r.data.Chat[chatKey("chat", instructions, prompt, options)]
	if !ok {
		return ollama.GenerationResult{}, missingFixture("chat", summarizePrompt(instructions, prompt))
	}
	if fixture.Error != "" {
		return fixture.Result, errors.New(fixture.Error)
	}
	return fixture.Result, nil
}

func (r *replayClient) StreamChat(_ context.Context, instructions, prompt string, options ollama.ChatOptions, onDelta func(string) error) (ollama.GenerationResult, error) {
	fixture, ok := r.data.Chat[chatKey("streamchat", instructions, prompt, options)]
	if !ok {
		return ollama.GenerationResult{}, missingFixture("streamchat", summarizePrompt(instructions, prompt))
	}
	if fixture.Error != "" {
		return fixture.Result, errors.New(fixture.Error)
	}
	if onDelta != nil && fixture.Content != "" {
		if err := onDelta(fixture.Content); err != nil {
			return fixture.Result, err
		}
	}
	return fixture.Result, nil
}

func (r *replayClient) ModelShow(_ context.Context, model string) (map[string]any, error) {
	encoded, ok := r.data.Model["modelshow\x00"+model]
	if !ok {
		return map[string]any{}, nil
	}
	payload := map[string]any{}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return map[string]any{}, nil
	}
	return payload, nil
}

func missingFixture(kind, detail string) error {
	return fmt.Errorf("fixture incompleto: falta una respuesta %s (%s); regenera con `-record`", kind, detail)
}

func summarizePrompt(instructions, prompt string) string {
	return fmt.Sprintf("instrucciones=%q prompt~=%q", first([]string{instructions}), truncate(prompt, 80))
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return truncate(values[0], 80)
}

func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// thresholdSpec es evaluations/thresholds.json: pisos mínimos por métrica.
type thresholdSpec struct {
	Metrics map[string]float64 `json:"metrics"`
}

var requiredQualityMetrics = []string{
	"document_recall",
	"recall_at_k",
	"ndcg_at_k",
	"citation_valid_rate",
	"end_to_end_pass_rate",
	"query_rewrite_pass_rate",
}

// checkThresholds compara las métricas del informe con los pisos declarados.
// Devuelve el texto del resumen y si todas se cumplen.
func checkThresholds(metrics map[string]any, path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var spec thresholdSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", false, fmt.Errorf("thresholds %s ilegible: %w", path, err)
	}
	if len(spec.Metrics) == 0 {
		return "", false, fmt.Errorf("thresholds %s no declara métricas", path)
	}
	for _, name := range requiredQualityMetrics {
		floor, ok := spec.Metrics[name]
		if !ok {
			return "", false, fmt.Errorf("thresholds %s no declara la métrica obligatoria %s", path, name)
		}
		if math.IsNaN(floor) || math.IsInf(floor, 0) || floor < 0 || floor > 1 {
			return "", false, fmt.Errorf("thresholds %s declara un piso inválido para %s: %v", path, name, floor)
		}
	}
	names := make([]string, 0, len(spec.Metrics))
	for name := range spec.Metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	allPass := true
	for _, name := range names {
		floor := spec.Metrics[name]
		value, ok := metricFloat(metrics[name])
		status := "OK"
		if !ok {
			status, allPass = "SIN VALOR", false
			fmt.Fprintf(&builder, "  %-26s  piso %.3f  obtenido —        %s\n", name, floor, status)
			continue
		}
		if value+1e-9 < floor {
			status, allPass = "POR DEBAJO", false
		}
		fmt.Fprintf(&builder, "  %-26s  piso %.3f  obtenido %.3f  %s\n", name, floor, value, status)
	}
	return builder.String(), allPass, nil
}

func metricFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case *float64:
		if typed == nil {
			return 0, false
		}
		return *typed, true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}
