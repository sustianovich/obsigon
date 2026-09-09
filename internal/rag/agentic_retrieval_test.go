package rag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
)

type scriptedResearchGenerator struct {
	responses []ollama.GenerationResult
	calls     int
}

func (g *scriptedResearchGenerator) Chat(_ context.Context, _, _ string, options ollama.ChatOptions) (ollama.GenerationResult, error) {
	if g.calls >= len(g.responses) {
		return ollama.GenerationResult{Content: `{"tool":"finish","arguments":{"answer":"fin"}}`, CompletionTokens: 5}, nil
	}
	result := g.responses[g.calls]
	g.calls++
	if result.CompletionTokens == 0 {
		result.CompletionTokens = 5
	}
	if result.PromptTokens == 0 {
		result.PromptTokens = 10
	}
	_ = options
	return result, nil
}

func (g *scriptedResearchGenerator) StreamChat(context.Context, string, string, ollama.ChatOptions, func(string) error) (ollama.GenerationResult, error) {
	return ollama.GenerationResult{}, nil
}

type agentToolFixture struct {
	store     *db.Store
	retriever *Retriever
	vault     string
}

func newAgentToolFixture(t *testing.T) agentToolFixture {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.md": "---\nestado: aprobado\n---\n# Nota A\n\nIntroducción.\n\n## Decisión\n\nSe aprobó X con código ABC-42.\n\n### Motivo\n\nDepende de Y.\n\n## Cierre\n\nTexto final.",
		"b.md": "# Nota B\n\nLa nota B explica Y.",
		"c.md": "# Nota C\n\nLa nota C enlaza de vuelta a A.",
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(vault, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := db.Open(filepath.Join(root, "agent.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dates := []time.Time{
		time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC),
	}
	documents := []db.Document{
		{Path: "a.md", Title: "Nota A", SHA256: "a", MTime: float64(dates[0].Unix()), Metadata: map[string]any{"estado": "aprobado", "_tags": []string{"proyecto/x"}}, Links: []db.Link{{Target: "Nota B"}}, Chunks: []db.Chunk{{Index: 0, Heading: "Decisión", Content: "Se aprobó X con código ABC-42. Depende de Y.", Embedding: []float32{1, 0}}}},
		{Path: "b.md", Title: "Nota B", SHA256: "b", MTime: float64(dates[1].Unix()), Metadata: map[string]any{"estado": "vigente", "_tags": []string{"proyecto"}}, Chunks: []db.Chunk{{Index: 0, Heading: "Documento", Content: "La nota B explica Y.", Embedding: []float32{.9, .1}}}},
		{Path: "c.md", Title: "Nota C", SHA256: "c", MTime: float64(dates[2].Unix()), Metadata: map[string]any{"estado": "borrador", "_tags": []string{"otro"}}, Links: []db.Link{{Target: "Nota A"}}, Chunks: []db.Chunk{{Index: 0, Heading: "Documento", Content: "La nota C enlaza de vuelta a A.", Embedding: []float32{0, 1}}}},
	}
	for _, document := range documents {
		if err := store.ReplaceDocument(context.Background(), document); err != nil {
			t.Fatal(err)
		}
	}
	// B se creó después de A; volver a publicar A resuelve su wikilink.
	if err := store.ReplaceDocument(context.Background(), documents[0]); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{
		VaultPath: vault, EmbeddingModel: "nomic-embed-text", EmbeddingPrefixScheme: "auto",
		TopK: 4, MinSimilarity: .2, MinRelativeScore: .2, HybridSearchEnabled: true,
		HybridRRFK: 60, HybridSemanticWeight: 1, HybridLexicalWeight: .8,
		HybridCandidateMultiplier: 4, MMRLambda: .72, MaxChunksPerDocument: 2,
	}
	vectors := NewVectorStore(store, 1)
	retriever := &Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: &fixedQueryEmbedder{vector: []float32{1, 0}}, Reranker: NewNoopReranker()}
	return agentToolFixture{store: store, retriever: retriever, vault: vault}
}

func executeAgentTool(t *testing.T, toolbox *AgentToolbox, name string, arguments map[string]any) AgentToolResult {
	t.Helper()
	raw, _ := json.Marshal(arguments)
	result, err := toolbox.Execute(context.Background(), AgentToolCall{Name: name, Arguments: raw})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func TestAgentToolboxCoversOrthogonalVaultOperations(t *testing.T) {
	fixture := newAgentToolFixture(t)
	toolbox := &AgentToolbox{Retriever: fixture.retriever, Store: fixture.store, VaultPath: fixture.vault, BaseRequest: RetrievalRequest{TopK: 3, MinSimilarity: .2}, ResultLimit: 8}

	searched := executeAgentTool(t, toolbox, "search_notes", map[string]any{"query": "decisión X", "top_k": 2})
	if searched.Retrieval == nil || len(searched.Hits) == 0 {
		t.Fatalf("search_notes=%+v", searched)
	}
	opened := executeAgentTool(t, toolbox, "open_note", map[string]any{"path": "a.md"})
	if len(opened.Hits) != 1 || !strings.Contains(opened.Hits[0]["content"].(string), "Texto final") || !strings.Contains(opened.Hits[0]["content"].(string), "ESQUEMA DE LA NOTA") {
		t.Fatalf("open_note=%+v", opened)
	}
	section := executeAgentTool(t, toolbox, "read_section", map[string]any{"path": "a.md", "heading": "Decisión"})
	sectionText := section.Hits[0]["content"].(string)
	if !strings.Contains(sectionText, "Depende de Y") || strings.Contains(sectionText, "Texto final") {
		t.Fatalf("sección=%q", sectionText)
	}
	neighbors := executeAgentTool(t, toolbox, "neighbors", map[string]any{"path": "a.md"})
	if items := neighbors.Payload.([]db.NoteNeighbor); len(items) != 2 || items[0].Direction != "outgoing" || items[1].Direction != "backlink" {
		t.Fatalf("neighbors=%+v", items)
	}
	if payload := executeAgentTool(t, toolbox, "search_by_tag", map[string]any{"tag": "proyecto"}).Payload.([]map[string]any); len(payload) != 2 {
		t.Fatalf("tags=%+v", payload)
	}
	if payload := executeAgentTool(t, toolbox, "search_by_frontmatter", map[string]any{"key": "estado", "value": "aprobado"}).Payload.([]map[string]any); len(payload) != 1 || payload[0]["path"] != "a.md" {
		t.Fatalf("frontmatter=%+v", payload)
	}
	if payload := executeAgentTool(t, toolbox, "recent", map[string]any{"n": 1}).Payload.([]map[string]any); len(payload) != 1 || payload[0]["path"] != "c.md" {
		t.Fatalf("recent=%+v", payload)
	}
	if payload := executeAgentTool(t, toolbox, "by_date", map[string]any{"from": "2026-01-03", "to": "2026-01-04"}).Payload.([]map[string]any); len(payload) != 2 {
		t.Fatalf("by_date=%+v", payload)
	}
	grep := executeAgentTool(t, toolbox, "grep", map[string]any{"regex": `ABC-\d+`})
	if len(grep.Hits) != 1 || !strings.Contains(grep.Hits[0]["content"].(string), "ABC-42") {
		t.Fatalf("grep=%+v", grep)
	}
	finished := executeAgentTool(t, toolbox, "finish", map[string]any{"answer": "Borrador comprobado"})
	if !finished.Finished || finished.FinalDraft != "Borrador comprobado" {
		t.Fatalf("finish=%+v", finished)
	}
	filtered := *toolbox
	filtered.BaseRequest.Status = "aprobado"
	if payload := executeAgentTool(t, &filtered, "recent", map[string]any{"n": 3}).Payload.([]map[string]any); len(payload) != 1 || payload[0]["path"] != "a.md" {
		t.Fatalf("los filtros base no se conservaron: %+v", payload)
	}
	raw, _ := json.Marshal(map[string]any{"path": "b.md"})
	if _, err := filtered.Execute(context.Background(), AgentToolCall{Name: "open_note", Arguments: raw}); err == nil {
		t.Fatal("open_note eludió el filtro de estado")
	}
}

func TestAgenticRetrievalDecomposesLoopsAndKeepsLargeEvidence(t *testing.T) {
	fixture := newAgentToolFixture(t)
	generator := &scriptedResearchGenerator{responses: []ollama.GenerationResult{
		{Content: `{"subquestions":["¿Qué se decidió sobre X?","¿Cómo se relaciona X con Y?"]}`},
		{Content: `{"tool":"open_note","arguments":{"path":"a.md"}}`},
		{Content: `{"tool":"finish","arguments":{"answer":"X se aprobó y depende de Y."}}`},
	}}
	result, err := RunAgenticRetrieval(context.Background(), generator, fixture.retriever, RetrievalRequest{Question: "¿Qué decidí sobre X y cómo se relaciona con Y?", TopK: 2, MinSimilarity: .2}, AgenticRetrievalOptions{MaxIterations: 4, TokenBudget: 1200, MaxSubquestions: 3, ContextTokens: 8192, MaxObservationChars: 8000})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResearchBrief != "X se aprobó y depende de Y." || result.Retrieval.Trace.Version != "2-agentic" || result.Retrieval.Abstained {
		t.Fatalf("result=%+v", result)
	}
	largeEvidence := ""
	for _, hit := range result.Retrieval.Hits {
		if hit["heading"] == "Nota completa" {
			largeEvidence = hit["content"].(string)
		}
	}
	if largeEvidence == "" || !strings.Contains(largeEvidence, "Cierre") {
		t.Fatalf("hits=%+v", result.Retrieval.Hits)
	}
	metrics := result.Metrics
	if metrics["stop_reason"] != "finish" || metrics["finish_called"] != true || len(metrics["subquestions"].([]string)) != 2 || metrics["iterations"] != 2 {
		t.Fatalf("metrics=%+v", metrics)
	}
}

func TestAgentProtocolParsingAndVaultBoundary(t *testing.T) {
	call, err := ParseAgentToolCall("```json\n{\"name\":\"grep\",\"args\":{\"regex\":\"ABC\"}}\n```")
	if err != nil || call.Name != "grep" || !strings.Contains(string(call.Arguments), "ABC") {
		t.Fatalf("call=%+v err=%v", call, err)
	}
	if _, err := secureVaultPath(t.TempDir(), "../fuera.md"); err == nil {
		t.Fatal("se aceptó una ruta fuera del vault")
	}
}

func TestRetrieverSearchesChunksThenReadsCompleteSection(t *testing.T) {
	fixture := newAgentToolFixture(t)
	fixture.retriever.Settings.ReadLargeEnabled = true
	result, err := fixture.retriever.Retrieve(context.Background(), RetrievalRequest{Question: "ABC-42", TopK: 2, MinSimilarity: .2})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range result.Hits {
		if hit["path"] != "a.md" {
			continue
		}
		found = true
		content := hit["content"].(string)
		path := hit["heading_path"].([]string)
		if !strings.Contains(content, "### Motivo") || !strings.Contains(content, "Depende de Y") || strings.Contains(content, "Texto final") || len(path) != 2 || path[0] != "Nota A" || hit["context_expansion"] != "full_section" {
			t.Fatalf("hit=%+v", hit)
		}
	}
	if !found {
		t.Fatalf("no se recuperó a.md: %+v", result.Hits)
	}
}
