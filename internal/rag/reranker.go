package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"obsigon/internal/ollama"
)

const rerankBatchSize = 8

type Reranker interface {
	Rerank(context.Context, string, []map[string]any) ([]map[string]any, RerankOutcome)
	Availability(context.Context) RerankAvailability
}

type RerankOutcome struct {
	Status    string  `json:"status"`
	Backend   string  `json:"backend"`
	Available bool    `json:"available"`
	Scored    int     `json:"scored"`
	Moved     int     `json:"moved"`
	Weight    float64 `json:"weight,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

type RerankAvailability struct {
	Backend   string `json:"backend"`
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type noopReranker struct{}

func NewNoopReranker() Reranker { return noopReranker{} }
func (noopReranker) Rerank(_ context.Context, _ string, candidates []map[string]any) ([]map[string]any, RerankOutcome) {
	return candidates, RerankOutcome{Status: "disabled", Backend: "noop", Available: false}
}
func (noopReranker) Availability(context.Context) RerankAvailability {
	return RerankAvailability{Backend: "noop", Enabled: false, Available: false, Reason: "Reranking desactivado."}
}

type rerankGenerator interface {
	Chat(context.Context, string, string, ollama.ChatOptions) (ollama.GenerationResult, error)
}

type llmReranker struct {
	generator  rerankGenerator
	enabled    bool
	candidates int
	weight     float64
}

type crossEncoderReranker struct {
	baseURL    string
	model      string
	client     *http.Client
	enabled    bool
	candidates int
	weight     float64
}

func NewCrossEncoderReranker(baseURL, model string, enabled bool, candidates int, weight float64, timeout time.Duration) Reranker {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &crossEncoderReranker{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), model: strings.TrimSpace(model), client: &http.Client{Timeout: timeout}, enabled: enabled, candidates: max(1, candidates), weight: clamp01(weight)}
}

func NewLLMReranker(generator rerankGenerator, enabled bool, candidates int, weight float64) Reranker {
	return &llmReranker{generator: generator, enabled: enabled, candidates: max(1, candidates), weight: clamp01(weight)}
}

func (r *llmReranker) Availability(ctx context.Context) RerankAvailability {
	result := RerankAvailability{Backend: "llm", Enabled: r.enabled}
	if r.generator == nil {
		result.Reason = "El cliente de Ollama no está configurado."
		return result
	}
	inspector, ok := r.generator.(interface {
		Status(context.Context) map[string]any
	})
	if !ok {
		result.Reason = "No se puede comprobar Ollama."
		return result
	}
	status := inspector.Status(ctx)
	result.Available, _ = status["accessible"].(bool)
	if !result.Available {
		result.Reason = "Ollama local no responde."
	}
	return result
}

func (r *llmReranker) Rerank(ctx context.Context, question string, candidates []map[string]any) ([]map[string]any, RerankOutcome) {
	base := RerankOutcome{Backend: "llm", Weight: r.weight}
	if !r.enabled {
		base.Status = "disabled"
		return candidates, base
	}
	if r.generator == nil || len(candidates) == 0 {
		base.Status = "not_run"
		base.Reason = "No hay candidatos o generador disponible."
		return candidates, base
	}
	limit := min(r.candidates, len(candidates))
	scores := make([]float64, limit)
	for start := 0; start < limit; start += rerankBatchSize {
		end := min(limit, start+rerankBatchSize)
		batchScores, err := r.scoreBatch(ctx, question, candidates[start:end], start)
		if err != nil {
			base.Status = "degraded"
			base.Reason = "Se conservó el orden previo porque el reranker no pudo puntuar la consulta."
			return candidates, base
		}
		base.Available = true
		copy(scores[start:end], batchScores)
	}

	type ranked struct {
		original int
		item     map[string]any
		combined float64
	}
	rankedItems := make([]ranked, limit)
	for index := 0; index < limit; index++ {
		item := cloneMap(candidates[index])
		fusion := clamp01(floatValue(item["fusion_score"]))
		combined := (1-r.weight)*fusion + r.weight*scores[index]
		item["rerank_score"] = scores[index]
		item["rerank_combined_score"] = combined
		rankedItems[index] = ranked{original: index, item: item, combined: combined}
	}
	sort.SliceStable(rankedItems, func(i, j int) bool { return rankedItems[i].combined > rankedItems[j].combined })
	result := make([]map[string]any, 0, len(candidates))
	moved := 0
	for index, item := range rankedItems {
		if item.original != index {
			moved++
		}
		result = append(result, item.item)
	}
	result = append(result, candidates[limit:]...)
	base.Status, base.Scored, base.Moved = "completed", limit, moved
	return result, base
}

func (r *crossEncoderReranker) Availability(ctx context.Context) RerankAvailability {
	result := RerankAvailability{Backend: "cross_encoder", Enabled: r.enabled}
	if !r.enabled {
		result.Reason = "Reranking cross-encoder desactivado."
		return result
	}
	if r.baseURL == "" {
		result.Reason = "Runtime cross-encoder no configurado."
		return result
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/health", nil)
	if err != nil {
		result.Reason = "Runtime cross-encoder no válido."
		return result
	}
	response, err := r.client.Do(request)
	if err != nil {
		result.Reason = "Runtime cross-encoder local no responde."
		return result
	}
	defer response.Body.Close()
	result.Available = response.StatusCode >= 200 && response.StatusCode < 300
	if !result.Available {
		result.Reason = "El healthcheck del cross-encoder no está disponible."
	}
	return result
}

func (r *crossEncoderReranker) Rerank(ctx context.Context, question string, candidates []map[string]any) ([]map[string]any, RerankOutcome) {
	base := RerankOutcome{Backend: "cross_encoder", Weight: r.weight}
	if !r.enabled {
		base.Status = "disabled"
		return candidates, base
	}
	if r.baseURL == "" || len(candidates) == 0 {
		base.Status = "not_run"
		base.Reason = "No hay candidatos o runtime cross-encoder disponible."
		return candidates, base
	}
	limit := min(r.candidates, len(candidates))
	scores, err := r.score(ctx, question, candidates[:limit])
	if err != nil {
		base.Status = "degraded"
		base.Reason = "Se conservó el orden previo porque el cross-encoder local no pudo puntuar la consulta."
		return candidates, base
	}
	result, moved := applyRerankScores(candidates, scores, limit, r.weight)
	base.Status, base.Available, base.Scored, base.Moved = "completed", true, limit, moved
	return result, base
}

func (r *crossEncoderReranker) score(ctx context.Context, question string, candidates []map[string]any) ([]float64, error) {
	type document struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	}
	documents := make([]document, len(candidates))
	for index, candidate := range candidates {
		content := stringValue(candidate["matched_content"])
		if content == "" {
			content = stringValue(candidate["content"])
		}
		heading := strings.Join(stringSlice(candidate["heading_path"]), " > ")
		if heading == "" {
			heading = stringValue(candidate["heading"])
		}
		documents[index] = document{ID: index, Text: truncateRunes(strings.Join([]string{stringValue(candidate["title"]), heading, content}, "\n"), 6000)}
	}
	payload := map[string]any{"model": r.model, "query": truncateRunes(strings.TrimSpace(question), 4000), "documents": documents}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/rerank", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("cross-encoder devolvió HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Scores []struct {
			ID    int     `json:"id"`
			Score float64 `json:"score"`
		} `json:"scores"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, err
	}
	scores := make([]float64, len(candidates))
	seen := make([]bool, len(candidates))
	for _, item := range envelope.Scores {
		if item.ID < 0 || item.ID >= len(scores) || seen[item.ID] || item.Score < 0 || item.Score > 1 {
			return nil, fmt.Errorf("puntuación cross-encoder no válida")
		}
		seen[item.ID], scores[item.ID] = true, item.Score
	}
	for _, item := range envelope.Results {
		if item.Index < 0 || item.Index >= len(scores) || seen[item.Index] || item.RelevanceScore < 0 || item.RelevanceScore > 1 {
			return nil, fmt.Errorf("puntuación cross-encoder no válida")
		}
		seen[item.Index], scores[item.Index] = true, item.RelevanceScore
	}
	for _, present := range seen {
		if !present {
			return nil, fmt.Errorf("faltan puntuaciones cross-encoder")
		}
	}
	return scores, nil
}

func applyRerankScores(candidates []map[string]any, scores []float64, limit int, weight float64) ([]map[string]any, int) {
	type ranked struct {
		original int
		item     map[string]any
		combined float64
	}
	rankedItems := make([]ranked, limit)
	for index := 0; index < limit; index++ {
		item := cloneMap(candidates[index])
		fusion := clamp01(floatValue(item["fusion_score"]))
		combined := (1-weight)*fusion + weight*scores[index]
		item["rerank_score"] = scores[index]
		item["rerank_combined_score"] = combined
		rankedItems[index] = ranked{original: index, item: item, combined: combined}
	}
	sort.SliceStable(rankedItems, func(i, j int) bool { return rankedItems[i].combined > rankedItems[j].combined })
	result := make([]map[string]any, 0, len(candidates))
	moved := 0
	for index, item := range rankedItems {
		if item.original != index {
			moved++
		}
		result = append(result, item.item)
	}
	result = append(result, candidates[limit:]...)
	return result, moved
}

func (r *llmReranker) scoreBatch(ctx context.Context, question string, candidates []map[string]any, offset int) ([]float64, error) {
	type promptCandidate struct {
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Heading string `json:"heading"`
		Text    string `json:"text"`
	}
	payload := make([]promptCandidate, len(candidates))
	for index, candidate := range candidates {
		content := stringValue(candidate["matched_content"])
		if content == "" {
			content = stringValue(candidate["content"])
		}
		heading := strings.Join(stringSlice(candidate["heading_path"]), " > ")
		if heading == "" {
			heading = stringValue(candidate["heading"])
		}
		payload[index] = promptCandidate{ID: offset + index, Title: stringValue(candidate["title"]), Heading: heading, Text: truncateRunes(content, 4000)}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf("Pregunta: %s\n\nFragmentos JSON:\n%s\n\nDevuelve únicamente JSON con la forma {\"scores\":[{\"id\":0,\"score\":0.0}]}. Incluye todos los ids exactamente una vez.", truncateRunes(strings.TrimSpace(question), 4000), encoded)
	generated, err := r.generator.Chat(ctx, "Puntúa la relevancia directa de cada fragmento para responder la pregunta. Usa números entre 0 y 1. No expliques la decisión.", prompt, ollama.ChatOptions{MaxOutputTokens: max(120, len(candidates)*24), Temperature: 0, Think: false})
	if err != nil {
		return nil, err
	}
	return parseRerankScores(generated.Content, offset, len(candidates))
}

func parseRerankScores(value string, offset, count int) ([]float64, error) {
	type score struct {
		ID    int     `json:"id"`
		Score float64 `json:"score"`
	}
	var envelope struct {
		Scores []score `json:"scores"`
	}
	raw := strings.TrimSpace(value)
	if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end >= start {
		raw = raw[start : end+1]
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil || len(envelope.Scores) != count {
		return nil, fmt.Errorf("formato de puntuaciones no válido")
	}
	result := make([]float64, count)
	seen := make([]bool, count)
	for _, item := range envelope.Scores {
		index := item.ID - offset
		if index < 0 || index >= count || seen[index] || item.Score < 0 || item.Score > 1 {
			return nil, fmt.Errorf("puntuación no válida")
		}
		seen[index], result[index] = true, item.Score
	}
	return result, nil
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
