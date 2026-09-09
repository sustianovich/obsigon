package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"obsigon/internal/db"
)

var markdownOutlineRE = regexp.MustCompile(`(?m)^(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)

type AgentToolCall struct {
	Name      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type AgentToolResult struct {
	Tool       string
	Summary    string
	Payload    any
	Hits       []map[string]any
	Retrieval  *RetrievalResult
	Finished   bool
	FinalDraft string
}

type AgentToolbox struct {
	Retriever   *Retriever
	Store       *db.Store
	VaultPath   string
	BaseRequest RetrievalRequest
	ResultLimit int
}

func (t *AgentToolbox) Execute(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
	call.Name = strings.ToLower(strings.TrimSpace(call.Name))
	if call.Name == "finish" {
		var args struct {
			Answer string `json:"answer"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Tool: call.Name, Finished: true, FinalDraft: compactText(args.Answer, 12000), Summary: "investigación finalizada"}, nil
	}
	if t.Store == nil {
		return AgentToolResult{}, errors.New("índice SQLite no configurado")
	}
	limit := t.ResultLimit
	if limit <= 0 {
		limit = 8
	}
	switch call.Name {
	case "search_notes":
		return t.searchNotes(ctx, call.Arguments, limit)
	case "open_note":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		return t.openNote(ctx, args.Path)
	case "read_section":
		var args struct {
			Path    string `json:"path"`
			Heading string `json:"heading"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		return t.readSection(ctx, args.Path, args.Heading)
	case "neighbors":
		var args struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		args.Limit = boundedToolLimit(args.Limit, limit)
		items, err := t.Store.NoteNeighbors(ctx, args.Path, 20)
		if err != nil {
			return AgentToolResult{}, err
		}
		items = capAgentSlice(t.filterNeighbors(items), args.Limit)
		return AgentToolResult{Tool: call.Name, Summary: fmt.Sprintf("%d notas enlazadas", len(items)), Payload: items}, nil
	case "search_by_tag":
		var args struct {
			Tag   string `json:"tag"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		requested := boundedToolLimit(args.Limit, limit)
		items, err := t.Store.NotesByTag(ctx, args.Tag, 20)
		if err != nil {
			return AgentToolResult{}, err
		}
		items = capAgentSlice(t.filterNotes(items), requested)
		return AgentToolResult{Tool: call.Name, Summary: fmt.Sprintf("%d notas con el tag", len(items)), Payload: publicNoteRecords(items)}, nil
	case "search_by_frontmatter":
		var args struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		requested := boundedToolLimit(args.Limit, limit)
		items, err := t.Store.NotesByFrontmatter(ctx, args.Key, args.Value, 20)
		if err != nil {
			return AgentToolResult{}, err
		}
		items = capAgentSlice(t.filterNotes(items), requested)
		return AgentToolResult{Tool: call.Name, Summary: fmt.Sprintf("%d notas con la propiedad", len(items)), Payload: publicNoteRecords(items)}, nil
	case "recent":
		var args struct {
			N int `json:"n"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		requested := boundedToolLimit(args.N, limit)
		items, err := t.Store.RecentNotes(ctx, 20)
		if err != nil {
			return AgentToolResult{}, err
		}
		items = capAgentSlice(t.filterNotes(items), requested)
		return AgentToolResult{Tool: call.Name, Summary: fmt.Sprintf("%d notas recientes", len(items)), Payload: publicNoteRecords(items)}, nil
	case "by_date":
		return t.byDate(ctx, call.Arguments, limit)
	case "grep":
		var args struct {
			Regex string `json:"regex"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return AgentToolResult{}, err
		}
		requested := boundedToolLimit(args.Limit, limit)
		matches, err := t.Store.GrepNotes(ctx, args.Regex, 20)
		if err != nil {
			return AgentToolResult{}, err
		}
		filtered := matches[:0]
		for _, match := range matches {
			if t.noteAllowed(match.NoteRecord) {
				filtered = append(filtered, match)
			}
		}
		matches = capAgentSlice(filtered, requested)
		hits := make([]map[string]any, 0, len(matches))
		for _, match := range matches {
			hits = append(hits, agentEvidenceHit(match.ID, match.Path, match.Title, match.Heading, match.Content, match.Metadata, "grep exacto"))
		}
		return AgentToolResult{Tool: call.Name, Summary: fmt.Sprintf("%d coincidencias exactas", len(matches)), Payload: matches, Hits: hits}, nil
	default:
		return AgentToolResult{}, fmt.Errorf("herramienta desconocida: %s", call.Name)
	}
}

func (t *AgentToolbox) searchNotes(ctx context.Context, raw json.RawMessage, defaultLimit int) (AgentToolResult, error) {
	if t.Retriever == nil {
		return AgentToolResult{}, errors.New("recuperador híbrido no configurado")
	}
	var args struct {
		Query       string         `json:"query"`
		Status      string         `json:"status"`
		Vigencia    string         `json:"vigencia"`
		Tags        []string       `json:"tags"`
		Frontmatter map[string]any `json:"frontmatter"`
		TopK        int            `json:"top_k"`
		ExpandLinks *bool          `json:"expand_links"`
	}
	if err := decodeToolArguments(raw, &args); err != nil {
		return AgentToolResult{}, err
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return AgentToolResult{}, errors.New("search_notes requiere query")
	}
	request := t.BaseRequest
	request.Frontmatter = make(map[string]any, len(t.BaseRequest.Frontmatter)+len(args.Frontmatter))
	for key, value := range t.BaseRequest.Frontmatter {
		request.Frontmatter[key] = value
	}
	request.Question, request.SemanticQuestion = args.Query, args.Query
	request.TopK = boundedToolLimit(args.TopK, defaultLimit)
	if request.Status == "" {
		request.Status = args.Status
	}
	if request.Vigencia == "" {
		request.Vigencia = args.Vigencia
	}
	request.Tags = normalizeTagFilters(append(request.Tags, args.Tags...))
	for key, value := range args.Frontmatter {
		if _, constrained := request.Frontmatter[key]; !constrained && !strings.HasPrefix(strings.TrimSpace(key), "_") {
			request.Frontmatter[key] = value
		}
	}
	if args.ExpandLinks != nil {
		request.ExpandLinks = *args.ExpandLinks
	}
	result, err := t.Retriever.Retrieve(ctx, request)
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Tool: "search_notes", Summary: fmt.Sprintf("%d fragmentos recuperados", len(result.Hits)), Payload: agentHitCards(result.Hits), Hits: result.Hits, Retrieval: &result}, nil
}

func (t *AgentToolbox) openNote(ctx context.Context, path string) (AgentToolResult, error) {
	note, body, err := t.readIndexedBody(ctx, path)
	if err != nil {
		return AgentToolResult{}, err
	}
	outline := markdownOutline(body)
	content := strings.TrimSpace(body)
	if len(outline) > 0 {
		content = "ESQUEMA DE LA NOTA\n" + strings.Join(outline, "\n") + "\n\nCONTENIDO COMPLETO\n" + content
	}
	hit := agentEvidenceHit(note.ID, note.Path, note.Title, "Nota completa", content, note.Metadata, "open_note (nota completa)")
	payload := map[string]any{"path": note.Path, "title": note.Title, "metadata": note.Metadata, "outline": outline, "content": body}
	return AgentToolResult{Tool: "open_note", Summary: fmt.Sprintf("nota completa abierta: %s", note.Path), Payload: payload, Hits: []map[string]any{hit}}, nil
}

func (t *AgentToolbox) readSection(ctx context.Context, path, heading string) (AgentToolResult, error) {
	heading = strings.TrimSpace(heading)
	if heading == "" {
		return AgentToolResult{}, errors.New("read_section requiere heading")
	}
	note, body, err := t.readIndexedBody(ctx, path)
	if err != nil {
		return AgentToolResult{}, err
	}
	actualHeading, section, ok := extractMarkdownSection(body, heading)
	if !ok {
		return AgentToolResult{}, fmt.Errorf("no existe la sección %q en %s", heading, note.Path)
	}
	hit := agentEvidenceHit(note.ID, note.Path, note.Title, actualHeading, section, note.Metadata, "read_section (sección completa)")
	payload := map[string]any{"path": note.Path, "title": note.Title, "heading": actualHeading, "content": section}
	return AgentToolResult{Tool: "read_section", Summary: fmt.Sprintf("sección completa leída: %s#%s", note.Path, actualHeading), Payload: payload, Hits: []map[string]any{hit}}, nil
}

func (t *AgentToolbox) byDate(ctx context.Context, raw json.RawMessage, defaultLimit int) (AgentToolResult, error) {
	var args struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Limit int    `json:"limit"`
	}
	if err := decodeToolArguments(raw, &args); err != nil {
		return AgentToolResult{}, err
	}
	from, err := parseAgentDate(args.From, false)
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("from: %w", err)
	}
	to, err := parseAgentDate(args.To, true)
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("to: %w", err)
	}
	requested := boundedToolLimit(args.Limit, defaultLimit)
	items, err := t.Store.NotesByMTime(ctx, from, to, 20)
	if err != nil {
		return AgentToolResult{}, err
	}
	items = capAgentSlice(t.filterNotes(items), requested)
	return AgentToolResult{Tool: "by_date", Summary: fmt.Sprintf("%d notas en el intervalo", len(items)), Payload: publicNoteRecords(items)}, nil
}

func (t *AgentToolbox) readIndexedBody(ctx context.Context, pathOrTitle string) (db.NoteRecord, string, error) {
	note, err := t.Store.FindNote(ctx, pathOrTitle)
	if err != nil {
		return db.NoteRecord{}, "", err
	}
	if !t.noteAllowed(note) {
		return db.NoteRecord{}, "", errors.New("la nota no cumple los filtros de la consulta")
	}
	path, pathErr := secureVaultPath(t.VaultPath, note.Path)
	if pathErr == nil {
		if data, readErr := os.ReadFile(path); readErr == nil {
			_, body := ParseFrontmatter(strings.ToValidUTF8(string(data), "�"))
			return note, strings.TrimSpace(body), nil
		}
	}
	sections, err := t.Store.IndexedNoteSections(ctx, note.ID)
	if err != nil {
		return db.NoteRecord{}, "", err
	}
	if len(sections) == 0 {
		if pathErr != nil {
			return db.NoteRecord{}, "", pathErr
		}
		return db.NoteRecord{}, "", fmt.Errorf("la nota %s no tiene contenido indexado", note.Path)
	}
	blocks := make([]string, 0, len(sections))
	for _, section := range sections {
		blocks = append(blocks, "## "+section[0]+"\n\n"+section[1])
	}
	return note, strings.Join(blocks, "\n\n"), nil
}

func secureVaultPath(vaultPath, indexedPath string) (string, error) {
	if strings.TrimSpace(vaultPath) == "" {
		return "", errors.New("ruta del vault no configurada")
	}
	relative := filepath.Clean(filepath.FromSlash(strings.TrimSpace(indexedPath)))
	if relative == "." || filepath.IsAbs(relative) {
		return "", errors.New("ruta de nota no válida")
	}
	root, err := filepath.Abs(vaultPath)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	inside, err := filepath.Rel(root, target)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", errors.New("la nota queda fuera del vault")
	}
	return target, nil
}

func markdownOutline(body string) []string {
	matches := markdownOutlineRE.FindAllStringSubmatch(body, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		result = append(result, strings.Repeat("  ", len(match[1])-1)+"- "+strings.TrimSpace(match[2]))
	}
	return result
}

func extractMarkdownSection(body, wanted string) (string, string, bool) {
	indices := markdownOutlineRE.FindAllStringSubmatchIndex(body, -1)
	wanted = normalizeLexical(strings.TrimSpace(strings.TrimLeft(wanted, "#")))
	for index, match := range indices {
		heading := strings.TrimSpace(body[match[4]:match[5]])
		if normalizeLexical(heading) != wanted {
			continue
		}
		level := match[3] - match[2]
		end := len(body)
		for next := index + 1; next < len(indices); next++ {
			nextLevel := indices[next][3] - indices[next][2]
			if nextLevel <= level {
				end = indices[next][0]
				break
			}
		}
		content := strings.TrimSpace(body[match[1]:end])
		return heading, content, true
	}
	return "", "", false
}

func agentEvidenceHit(documentID int64, path, title, heading, content string, metadata map[string]any, reason string) map[string]any {
	return map[string]any{
		"chunk_id": int64(0), "document_id": documentID, "path": path, "title": title,
		"heading": heading, "content": strings.TrimSpace(content), "matched_content": nil,
		"metadata": metadata, "reason": reason, "score": float64(0),
		"semantic_score": nil, "lexical_score": nil, "graph_score": nil, "fusion_score": nil,
		"mmr_score": nil, "rerank_score": nil, "rerank_combined_score": nil,
		"context_expanded": true, "matched_chunk_ids": []int64{},
		"tags": keys(metadataTags(metadata)), "vigencia": metadataVigenciaDetails(metadata),
	}
}

func agentHitCards(hits []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		result = append(result, map[string]any{
			"path": hit["path"], "title": hit["title"], "heading": hit["heading"],
			"heading_path": hit["heading_path"], "outline": hit["outline"], "modified_at": hit["mtime"],
			"reason": hit["reason"], "score": hit["score"], "content": compactText(fmt.Sprint(hit["content"]), 1200),
		})
	}
	return result
}

func publicNoteRecords(notes []db.NoteRecord) []map[string]any {
	result := make([]map[string]any, 0, len(notes))
	for _, note := range notes {
		result = append(result, map[string]any{"path": note.Path, "title": note.Title, "modified_at": time.Unix(int64(note.MTime), 0).UTC().Format(time.RFC3339), "metadata": note.Metadata})
	}
	return result
}

func decodeToolArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("argumentos no válidos: %w", err)
	}
	return nil
}

func boundedToolLimit(value, fallback int) int {
	if fallback <= 0 {
		fallback = 8
	}
	if value <= 0 {
		value = fallback
	}
	return min(20, max(1, value))
}

func parseAgentDate(value string, endOfDay bool) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
	}
	if err != nil {
		return 0, errors.New("se esperaba YYYY-MM-DD o RFC3339")
	}
	if endOfDay && len(value) == len("2006-01-02") {
		parsed = parsed.Add(24*time.Hour - time.Nanosecond)
	}
	return float64(parsed.UnixNano()) / 1e9, nil
}

func (t *AgentToolbox) noteAllowed(note db.NoteRecord) bool {
	status := strings.ToLower(strings.TrimSpace(t.BaseRequest.Status))
	if status != "" && metadataStatus(note.Metadata) != status {
		return false
	}
	vigencia := strings.ToLower(strings.TrimSpace(t.BaseRequest.Vigencia))
	actualVigencia := metadataVigencia(note.Metadata)
	if vigencia != "" && !(vigencia == "no_caducada" && actualVigencia != "caducada") && actualVigencia != vigencia {
		return false
	}
	available := metadataTags(note.Metadata)
	for _, tag := range normalizeTagFilters(t.BaseRequest.Tags) {
		if !available[tag] {
			return false
		}
	}
	return metadataMatchesFrontmatter(note.Metadata, t.BaseRequest.Frontmatter)
}

func (t *AgentToolbox) filterNotes(notes []db.NoteRecord) []db.NoteRecord {
	result := notes[:0]
	for _, note := range notes {
		if t.noteAllowed(note) {
			result = append(result, note)
		}
	}
	return result
}

func (t *AgentToolbox) filterNeighbors(notes []db.NoteNeighbor) []db.NoteNeighbor {
	result := notes[:0]
	for _, note := range notes {
		if t.noteAllowed(note.NoteRecord) {
			result = append(result, note)
		}
	}
	return result
}

func capAgentSlice[T any](items []T, limit int) []T {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func sortedToolNames() []string {
	result := []string{"by_date", "finish", "grep", "neighbors", "open_note", "read_section", "recent", "search_by_frontmatter", "search_by_tag", "search_notes"}
	sort.Strings(result)
	return result
}
