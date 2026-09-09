package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestChatModelCanChangeWhileRequestsAreBuilt(t *testing.T) {
	client := New("http://127.0.0.1:11434", "modelo-a", "embed", "5m", time.Second, 1)
	allowed := map[string]bool{"modelo-a": true, "modelo-b": true}
	errors := make(chan string, 1)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(offset int) {
			defer wait.Done()
			for index := 0; index < 1000; index++ {
				if (index+offset)%2 == 0 {
					client.SetChatModel("modelo-a")
				} else {
					client.SetChatModel("modelo-b")
				}
				if model := client.chatRequest("", "", false, ChatOptions{}).Model; !allowed[model] {
					select {
					case errors <- model:
					default:
					}
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	select {
	case model := <-errors:
		t.Fatalf("modelo inconsistente: %q", model)
	default:
	}
}

func TestEmbeddingsKeepOrderAndContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		var body embedRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "modelo" || !body.Truncate || body.KeepAlive != "30m" {
			t.Errorf("payload=%+v", body)
		}
		vectors := make([][]float32, len(body.Input))
		for index := range body.Input {
			vectors[index] = []float32{float32(len(body.Input[index])), 1}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: vectors})
	}))
	defer server.Close()
	client := New(server.URL, "chat", "modelo", "30m", time.Second, 3)
	inputs := make([]string, 130)
	for index := range inputs {
		inputs[index] = string(make([]byte, index%17+1))
	}
	vectors, err := client.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(inputs) {
		t.Fatalf("vectores=%d", len(vectors))
	}
	for index := range inputs {
		if vectors[index][0] != float32(len(inputs[index])) {
			t.Fatalf("orden roto en %d", index)
		}
	}
}

func TestChatFiltersInternalThinkingAndReportsMeasuredTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != "qwen3:14b" || body["think"] != true || body["stream"] != false {
			t.Errorf("payload=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "<think>secreto</think>Respuesta [1]"}, "prompt_eval_count": 11, "eval_count": 4, "done": true})
	}))
	defer server.Close()
	client := New(server.URL, "qwen3:14b", "embed", "30m", time.Second, 1)
	result, err := client.Chat(context.Background(), "Sistema", "Pregunta", ChatOptions{MaxOutputTokens: 100, Temperature: .2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Respuesta [1]" || result.PromptTokens != 11 || result.CompletionTokens != 4 || result.Estimated {
		t.Fatalf("resultado=%+v", result)
	}
}

func TestStreamChatFiltersSplitThinkingTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := w.(http.Flusher)
		for _, payload := range []map[string]any{{"message": map[string]any{"content": "Ho"}}, {"message": map[string]any{"content": "la <th"}}, {"message": map[string]any{"content": "ink>x</thi"}}, {"message": map[string]any{"content": "nk> mundo"}}, {"done": true, "prompt_eval_count": 8, "eval_count": 3}} {
			_ = json.NewEncoder(w).Encode(payload)
			flusher.Flush()
		}
	}))
	defer server.Close()
	client := New(server.URL, "chat", "embed", "30m", time.Second, 1)
	visible := ""
	result, err := client.StreamChat(context.Background(), "Sistema", "Pregunta", ChatOptions{}, func(piece string) error { visible += piece; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if visible != "Hola  mundo" || result.Content != visible || result.PromptTokens != 8 || result.CompletionTokens != 3 {
		t.Fatalf("visible=%q result=%+v", visible, result)
	}
}

func TestUnclosedThinkingNeverLeaks(t *testing.T) {
	filter := thinkFilter{}
	if visible := filter.Feed("Antes<think>nunca cierra"); visible != "Antes" {
		t.Fatalf("visible=%q", visible)
	}
	if tail := filter.Flush(); tail != "" {
		t.Fatalf("se filtró razonamiento: %q", tail)
	}
}

func TestModelShowReadsLocalMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != "modelo-local" {
			t.Errorf("payload=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"llama.context_length": 8192}})
	}))
	defer server.Close()
	client := New(server.URL, "modelo-local", "embed", "30m", time.Second, 1)
	payload, err := client.ModelShow(context.Background(), "modelo-local")
	if err != nil {
		t.Fatal(err)
	}
	info := payload["model_info"].(map[string]any)
	if info["llama.context_length"].(float64) != 8192 {
		t.Fatalf("metadatos=%+v", payload)
	}
}

func TestStatusReportsInstalledModelsWithCanonicalNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "0.12.0"})
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "qwen3.5:0.8b"}, {"model": "nomic-embed-text:latest"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := New(server.URL, "qwen3.5:0.8b", "nomic-embed-text", "30m", time.Second, 1)
	status := client.Status(context.Background())
	if status["accessible"] != true || status["version"] != "0.12.0" {
		t.Fatalf("status=%+v", status)
	}
	if status["chat_model"].(map[string]any)["installed"] != true || status["embedding_model"].(map[string]any)["installed"] != true {
		t.Fatalf("modelos=%+v", status)
	}
}
