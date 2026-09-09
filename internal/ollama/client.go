// Package ollama contiene el cliente HTTP exclusivamente local.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const embeddingBatchSize = 64

var ErrUnavailable = errors.New("Ollama local no disponible")

type Client struct {
	BaseURL, Model, KeepAlive string
	Workers                   int
	HTTP                      *http.Client
	stateMu                   sync.RWMutex
	chatModel                 string
}

func New(baseURL, chatModel, embeddingModel, keepAlive string, timeout time.Duration, workers int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Model: embeddingModel, chatModel: strings.TrimSpace(chatModel), KeepAlive: keepAlive, Workers: max(1, workers), HTTP: &http.Client{Timeout: timeout, Transport: transport}}
}

func (c *Client) SetChatModel(model string) {
	c.stateMu.Lock()
	c.chatModel = strings.TrimSpace(model)
	c.stateMu.Unlock()
}

func (c *Client) ChatModelName() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.chatModel
}

type ChatOptions struct {
	MaxOutputTokens int
	Temperature     float64
	ContextWindow   int
	// Think fuerza el modo de razonamiento cuando no es nil. Los flujos
	// deterministas, como el reranker, lo desactivan explícitamente.
	Think any
}
type GenerationResult struct {
	Content          string `json:"content"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Estimated        bool   `json:"estimated"`
}

// ModelShow lee exclusivamente metadatos de un modelo ya instalado. No
// descarga ni activa modelos y conserva la barrera HTTP local de Client.
func (c *Client) ModelShow(ctx context.Context, model string) (map[string]any, error) {
	showContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"model": strings.TrimSpace(model)})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(showContext, http.MethodPost, c.BaseURL+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: no se pueden leer los metadatos: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Ollama devolvió HTTP %d al leer el modelo", response.StatusCode)
	}
	var decoded map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&decoded); err != nil {
		return nil, errors.New("Ollama devolvió metadatos que no son JSON válido")
	}
	return decoded, nil
}

// Status devuelve un contrato estable aunque Ollama esté apagado. Los
// detalles técnicos no salen de esta capa ni se exponen al navegador.
func (c *Client) Status(ctx context.Context) map[string]any {
	chatModel := c.ChatModelName()
	result := map[string]any{
		"accessible": false, "base_url": c.BaseURL, "version": nil,
		"chat_model":      map[string]any{"name": chatModel, "installed": nil},
		"embedding_model": map[string]any{"name": c.Model, "installed": nil},
		"error":           nil,
	}
	statusContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	version, err := c.getJSON(statusContext, "/api/version")
	if err != nil {
		result["error"] = "No se pudo consultar el estado de Ollama local."
		return result
	}
	result["accessible"] = true
	if value, ok := version["version"].(string); ok {
		result["version"] = value
	}
	models, err := c.getJSON(statusContext, "/api/tags")
	if err != nil {
		result["error"] = "No se pudo consultar el estado de Ollama local."
		return result
	}
	installed := map[string]bool{}
	items, ok := models["models"].([]any)
	if !ok {
		result["error"] = "Ollama devolvió un estado inesperado."
		return result
	}
	for _, item := range items {
		entry, _ := item.(map[string]any)
		for _, key := range []string{"name", "model"} {
			if name, ok := entry[key].(string); ok && strings.TrimSpace(name) != "" {
				installed[canonicalModelName(name)] = true
			}
		}
	}
	result["chat_model"].(map[string]any)["installed"] = installed[canonicalModelName(chatModel)]
	result["embedding_model"].(map[string]any)["installed"] = installed[canonicalModelName(c.Model)]
	return result
}

func (c *Client) getJSON(ctx context.Context, path string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Ollama devolvió HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func canonicalModelName(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	component := normalized
	if slash := strings.LastIndex(component, "/"); slash >= 0 {
		component = component[slash+1:]
	}
	if !strings.Contains(component, ":") {
		normalized += ":latest"
	}
	return normalized
}

func (c *Client) Chat(ctx context.Context, instructions, prompt string, options ChatOptions) (GenerationResult, error) {
	requestBody := c.chatRequest(instructions, prompt, false, options)
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return GenerationResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return GenerationResult{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return GenerationResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return GenerationResult{}, err
	}
	if response.StatusCode >= 400 {
		return GenerationResult{}, fmt.Errorf("Ollama devolvió HTTP %d", response.StatusCode)
	}
	var decoded chatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return GenerationResult{}, errors.New("Ollama devolvió una respuesta que no es JSON válido")
	}
	answer := stripInternalThinking(decoded.Message.Content)
	if strings.TrimSpace(answer) == "" {
		return GenerationResult{}, errors.New("Ollama no devolvió contenido para la respuesta")
	}
	promptTokens, promptMeasured := validCount(decoded.PromptEvalCount)
	completionTokens, completionMeasured := validCount(decoded.EvalCount)
	if !promptMeasured {
		promptTokens = estimateTokens(instructions + prompt)
	}
	if !completionMeasured {
		completionTokens = estimateTokens(answer)
	}
	return GenerationResult{Content: answer, PromptTokens: promptTokens, CompletionTokens: completionTokens, Estimated: !promptMeasured || !completionMeasured}, nil
}

func (c *Client) StreamChat(ctx context.Context, instructions, prompt string, options ChatOptions, onDelta func(string) error) (GenerationResult, error) {
	payload, err := json.Marshal(c.chatRequest(instructions, prompt, true, options))
	if err != nil {
		return GenerationResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return GenerationResult{}, err
	}
	request.Header.Set("Accept", "application/x-ndjson")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return GenerationResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return GenerationResult{}, fmt.Errorf("Ollama devolvió HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	filter := thinkFilter{}
	var answer strings.Builder
	promptTokens, completionTokens := 0, 0
	promptMeasured, completionMeasured := false, false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var decoded chatResponse
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			continue
		}
		if decoded.Message.Content != "" {
			visible := filter.Feed(decoded.Message.Content)
			if visible != "" {
				answer.WriteString(visible)
				if err := onDelta(visible); err != nil {
					return GenerationResult{}, err
				}
			}
		}
		if decoded.Done {
			promptTokens, promptMeasured = validCount(decoded.PromptEvalCount)
			completionTokens, completionMeasured = validCount(decoded.EvalCount)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return GenerationResult{}, err
	}
	if tail := filter.Flush(); tail != "" {
		answer.WriteString(tail)
		if err := onDelta(tail); err != nil {
			return GenerationResult{}, err
		}
	}
	if answer.Len() == 0 {
		return GenerationResult{}, errors.New("Ollama no devolvió contenido para la respuesta")
	}
	if !promptMeasured {
		promptTokens = estimateTokens(instructions + prompt)
	}
	if !completionMeasured {
		completionTokens = estimateTokens(answer.String())
	}
	return GenerationResult{Content: answer.String(), PromptTokens: promptTokens, CompletionTokens: completionTokens, Estimated: !promptMeasured || !completionMeasured}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model     string         `json:"model"`
	Messages  []chatMessage  `json:"messages"`
	Stream    bool           `json:"stream"`
	Think     any            `json:"think"`
	KeepAlive string         `json:"keep_alive"`
	Options   map[string]any `json:"options"`
}
type chatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool `json:"done"`
	PromptEvalCount any  `json:"prompt_eval_count"`
	EvalCount       any  `json:"eval_count"`
}

func (c *Client) chatRequest(instructions, prompt string, stream bool, options ChatOptions) chatRequest {
	chatModel := c.ChatModelName()
	if options.MaxOutputTokens <= 0 {
		options.MaxOutputTokens = 1800
	}
	requestOptions := map[string]any{"temperature": options.Temperature, "num_predict": options.MaxOutputTokens}
	if options.ContextWindow > 0 {
		requestOptions["num_ctx"] = options.ContextWindow
	}
	think := options.Think
	if think == nil {
		think = thinkValue(chatModel)
	}
	return chatRequest{Model: chatModel, Messages: []chatMessage{{Role: "system", Content: instructions}, {Role: "user", Content: prompt}}, Stream: stream, Think: think, KeepAlive: c.KeepAlive, Options: requestOptions}
}

func thinkValue(model string) any {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(normalized, "gpt-oss") {
		return "low"
	}
	if strings.HasPrefix(normalized, "qwen3:") {
		return true
	}
	return false
}
func validCount(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number >= 0 {
			return int(number), true
		}
	case int:
		if number >= 0 {
			return number, true
		}
	case json.Number:
		value, err := number.Int64()
		if err == nil && value >= 0 {
			return int(value), true
		}
	}
	return 0, false
}
func estimateTokens(value string) int {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	return max(1, int(math.Ceil(float64(len([]rune(value)))/4)))
}

var thinkBlockRE = regexp.MustCompile(`(?is)<think\b[^>]*>.*?</think\s*>`)

func stripInternalThinking(value string) string {
	return strings.TrimSpace(thinkBlockRE.ReplaceAllString(value, ""))
}

type thinkFilter struct {
	buffer string
	inside bool
}

func (f *thinkFilter) Feed(piece string) string {
	f.buffer += piece
	var output strings.Builder
	for f.buffer != "" {
		if f.inside {
			end := strings.Index(f.buffer, "</think>")
			if end < 0 {
				keep := len("</think>") - 1
				if len(f.buffer) > keep {
					f.buffer = f.buffer[len(f.buffer)-keep:]
				}
				break
			}
			f.buffer = f.buffer[end+len("</think>"):]
			f.inside = false
			continue
		}
		start := strings.Index(f.buffer, "<think")
		if start < 0 {
			keep := len("<think") - 1
			if len(f.buffer) > keep {
				output.WriteString(f.buffer[:len(f.buffer)-keep])
				f.buffer = f.buffer[len(f.buffer)-keep:]
			}
			break
		}
		output.WriteString(f.buffer[:start])
		closing := strings.Index(f.buffer[start:], ">")
		if closing < 0 {
			f.buffer = f.buffer[start:]
			break
		}
		f.buffer = f.buffer[start+closing+1:]
		f.inside = true
	}
	return output.String()
}
func (f *thinkFilter) Flush() string {
	if f.inside {
		f.buffer = ""
		return ""
	}
	remaining := f.buffer
	f.buffer = ""
	return remaining
}

type embedRequest struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	Truncate  bool     `json:"truncate"`
	KeepAlive string   `json:"keep_alive"`
}
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	batches := make([][]string, 0, (len(texts)+embeddingBatchSize-1)/embeddingBatchSize)
	for start := 0; start < len(texts); start += embeddingBatchSize {
		batches = append(batches, texts[start:min(len(texts), start+embeddingBatchSize)])
	}
	results := make([][][]float32, len(batches))
	workers := min(c.Workers, len(batches))
	jobs := make(chan int)
	var wait sync.WaitGroup
	var firstErr error
	var errorMu sync.Mutex
	worker := func() {
		defer wait.Done()
		for index := range jobs {
			vectors, err := c.embedBatch(ctx, batches[index])
			if err != nil {
				errorMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errorMu.Unlock()
				continue
			}
			results[index] = vectors
		}
	}
	wait.Add(workers)
	for range workers {
		go worker()
	}
	for index := range batches {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	vectors := make([][]float32, 0, len(texts))
	dimension := 0
	for _, batch := range results {
		if len(batch) > 0 {
			if dimension == 0 {
				dimension = len(batch[0])
			}
			if len(batch[0]) != dimension {
				return nil, errors.New("la dimensión de los embeddings cambió entre lotes")
			}
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

func (c *Client) Dimension(ctx context.Context) (int, error) {
	vectors, err := c.Embed(ctx, []string{"comprobación de dimensión"})
	if err != nil {
		return 0, err
	}
	if len(vectors) != 1 {
		return 0, errors.New("Ollama no devolvió el embedding de comprobación")
	}
	return len(vectors[0]), nil
}

func (c *Client) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	payload, err := json.Marshal(embedRequest{Model: c.Model, Input: batch, Truncate: true, KeepAlive: c.KeepAlive})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var decoded embedResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, errors.New("Ollama devolvió una respuesta que no es JSON válido")
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Ollama devolvió HTTP %d", response.StatusCode)
	}
	if len(decoded.Embeddings) != len(batch) {
		return nil, errors.New("Ollama devolvió un número de embeddings distinto al número de textos enviados")
	}
	dimension := 0
	for _, vector := range decoded.Embeddings {
		if len(vector) == 0 {
			return nil, errors.New("Ollama devolvió un embedding vacío")
		}
		if dimension == 0 {
			dimension = len(vector)
		} else if len(vector) != dimension {
			return nil, errors.New("Ollama devolvió embeddings con dimensiones diferentes")
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, errors.New("Ollama devolvió un embedding con valores no finitos")
			}
		}
	}
	return decoded.Embeddings, nil
}
