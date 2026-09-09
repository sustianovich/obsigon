package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	ErrNotFound       = errors.New("no encontrado")
	ErrDuplicate      = errors.New("valor duplicado")
	ErrDefaultProject = errors.New("el proyecto General no se puede eliminar")
)

type AgentSettings struct {
	VerificationEnabled   bool `json:"verification_enabled"`
	ProjectMemoryEnabled  bool `json:"project_memory_enabled"`
	VerifierContextTokens int  `json:"verifier_context_tokens"`
	WriterContextTokens   int  `json:"writer_context_tokens"`
}

func newID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func (s *Store) ProjectExists(ctx context.Context, id string) (bool, error) {
	var value int
	err := s.ReadDB().QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=?`, id).Scan(&value)
	if errors.Is(err, sqlErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Alias local para mantener explícitas las comprobaciones ErrNoRows sin
// exportar database/sql desde las firmas públicas.
var sqlErrNoRows = sql.ErrNoRows

func (s *Store) ListProjects(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT p.id,p.name,p.verification_enabled,p.project_memory_enabled,p.verifier_context_tokens,p.writer_context_tokens,p.created_at,p.updated_at,COUNT(c.id) FROM projects p LEFT JOIN conversations c ON c.project_id=p.id GROUP BY p.id ORDER BY CASE WHEN p.id=? THEN 0 ELSE 1 END,p.name COLLATE NOCASE`, DefaultProjectID)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		var id, name, createdAt, updatedAt string
		var verification, projectMemory, verifierTokens, writerTokens, count int
		if err := rows.Scan(&id, &name, &verification, &projectMemory, &verifierTokens, &writerTokens, &createdAt, &updatedAt, &count); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{
			"id": id, "name": name, "created_at": createdAt, "updated_at": updatedAt, "conversation_count": count,
			"agent_settings": AgentSettings{verification != 0, projectMemory != 0, verifierTokens, writerTokens},
			"context_usage":  emptyContextUsage(),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// SQLite se abre con una sola conexión para serializar escrituras. Las
	// consultas de memoria se hacen después de cerrar rows para no bloquear
	// esperando esa misma conexión.
	for _, project := range result {
		memory, err := s.ProjectMemoryStatus(ctx, project["id"].(string))
		if err != nil {
			return nil, err
		}
		project["memory"] = memory
		usage, err := s.ProjectContextUsage(ctx, project["id"].(string))
		if err != nil {
			return nil, err
		}
		project["context_usage"] = usage
	}
	return result, nil
}

func (s *Store) ProjectContextUsage(ctx context.Context, projectID string) (map[string]any, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT agent_metrics_json FROM (SELECT t.id,t.agent_metrics_json FROM conversation_turns t JOIN conversations c ON c.id=t.conversation_id WHERE c.project_id=? ORDER BY t.id DESC LIMIT 1000) ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := map[string][]map[string]any{"verifier": {}, "writer": {}}
	latest := map[string]any{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var metrics map[string]any
		if json.Unmarshal([]byte(raw), &metrics) != nil || metrics == nil {
			continue
		}
		latest = metrics
		for _, role := range []string{"verifier", "writer"} {
			if point := agentUsagePoint(metrics, role); point != nil {
				points[role] = append(points[role], point)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := emptyContextUsage()
	latestPercentages, maximumPercentages, allPercentages := []float64{}, []float64{}, []float64{}
	for _, role := range []string{"verifier", "writer"} {
		rolePoints := points[role]
		if len(rolePoints) == 0 {
			continue
		}
		percentages := make([]float64, 0, len(rolePoints))
		for _, point := range rolePoints {
			percentages = append(percentages, point["usage_percent"].(float64))
		}
		maximum, sum := percentages[0], 0.0
		for _, percent := range percentages {
			maximum = max(maximum, percent)
			sum += percent
		}
		latestPoint := agentUsagePoint(latest, role)
		if latestPoint != nil {
			latestPercentages = append(latestPercentages, latestPoint["usage_percent"].(float64))
		}
		maximumPercentages = append(maximumPercentages, maximum)
		allPercentages = append(allPercentages, percentages...)
		result[role] = map[string]any{"latest": latestPoint, "maximum_percent": roundUsage(maximum), "average_percent": roundUsage(sum / float64(len(percentages))), "samples": len(percentages)}
	}
	if len(latestPercentages) > 0 {
		result["latest_percent"] = roundUsage(maxSlice(latestPercentages))
	}
	if len(maximumPercentages) > 0 {
		result["maximum_percent"] = roundUsage(maxSlice(maximumPercentages))
	}
	if len(allPercentages) > 0 {
		sum := 0.0
		for _, value := range allPercentages {
			sum += value
		}
		result["average_percent"] = roundUsage(sum / float64(len(allPercentages)))
	}
	return result, nil
}

func agentUsagePoint(metrics map[string]any, role string) map[string]any {
	raw, _ := metrics[role].(map[string]any)
	status, _ := raw["status"].(string)
	if status != "completed" && status != "degraded" {
		return nil
	}
	window := int(numberFromJSON(raw["context_window_tokens"]))
	prompt := int(numberFromJSON(raw["prompt_tokens"]))
	completion := int(numberFromJSON(raw["completion_tokens"]))
	total := int(numberFromJSON(raw["total_tokens"]))
	if total == 0 {
		total = prompt + completion
	}
	if window <= 0 || total < 0 {
		return nil
	}
	return map[string]any{"context_window_tokens": window, "prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total, "usage_percent": roundUsage(min(100, float64(total)/float64(window)*100)), "estimated": raw["estimated"] == true}
}

func numberFromJSON(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return 0
	}
}

func maxSlice(values []float64) float64 {
	result := values[0]
	for _, value := range values[1:] {
		result = max(result, value)
	}
	return result
}

func roundUsage(value float64) float64 { return math.Round(value*100) / 100 }

func (s *Store) CreateProject(ctx context.Context, name string, verifierTokens, writerTokens int) (map[string]any, error) {
	name = normalizeName(name)
	if name == "" || len([]rune(name)) > 80 {
		return nil, fmt.Errorf("nombre de proyecto no válido")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := utcNow()
	_, err = s.WriteDB().ExecContext(ctx, `INSERT INTO projects(id,name,verification_enabled,project_memory_enabled,verifier_context_tokens,writer_context_tokens,created_at,updated_at) VALUES(?,?,1,1,?,?,?,?)`, id, name, verifierTokens, writerTokens, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return map[string]any{"id": id, "name": name, "created_at": now, "updated_at": now, "conversation_count": 0, "agent_settings": AgentSettings{true, true, verifierTokens, writerTokens}, "context_usage": emptyContextUsage()}, nil
}

func (s *Store) RenameProject(ctx context.Context, id, name string) (map[string]any, error) {
	name = normalizeName(name)
	if name == "" || len([]rune(name)) > 80 {
		return nil, fmt.Errorf("nombre de proyecto no válido")
	}
	now := utcNow()
	result, err := s.WriteDB().ExecContext(ctx, `UPDATE projects SET name=?,updated_at=? WHERE id=?`, name, now, id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return nil, ErrNotFound
	}
	var conversations int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE project_id=?`, id).Scan(&conversations); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "name": name, "updated_at": now, "conversation_count": conversations}, nil
}

func (s *Store) DeleteProject(ctx context.Context, id string) (map[string]any, error) {
	if id == DefaultProjectID {
		return nil, ErrDefaultProject
	}
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, id).Scan(&name); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE project_id=?`, id).Scan(&count); err != nil {
		return nil, err
	}
	now := utcNow()
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET project_id=?,updated_at=? WHERE project_id=?`, DefaultProjectID, now, id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_memory WHERE project_id=?`, id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET updated_at=? WHERE id=?`, now, DefaultProjectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "project_id": id, "project_name": name, "moved_conversations": count, "fallback_project_id": DefaultProjectID}, nil
}

func (s *Store) GetAgentSettings(ctx context.Context, projectID string) (AgentSettings, error) {
	var result AgentSettings
	var verification, memory int
	err := s.ReadDB().QueryRowContext(ctx, `SELECT verification_enabled,project_memory_enabled,verifier_context_tokens,writer_context_tokens FROM projects WHERE id=?`, projectID).Scan(&verification, &memory, &result.VerifierContextTokens, &result.WriterContextTokens)
	if errors.Is(err, sqlErrNoRows) {
		return AgentSettings{}, ErrNotFound
	}
	result.VerificationEnabled, result.ProjectMemoryEnabled = verification != 0, memory != 0
	return result, err
}

func (s *Store) UpdateAgentSettings(ctx context.Context, projectID string, settings AgentSettings) (AgentSettings, error) {
	if settings.VerifierContextTokens < 4096 || settings.VerifierContextTokens > 262144 || settings.WriterContextTokens < 4096 || settings.WriterContextTokens > 262144 {
		return AgentSettings{}, fmt.Errorf("ventana de contexto no válida")
	}
	result, err := s.WriteDB().ExecContext(ctx, `UPDATE projects SET verification_enabled=?,project_memory_enabled=?,verifier_context_tokens=?,writer_context_tokens=?,updated_at=? WHERE id=?`, boolInt(settings.VerificationEnabled), boolInt(settings.ProjectMemoryEnabled), settings.VerifierContextTokens, settings.WriterContextTokens, utcNow(), projectID)
	if err != nil {
		return AgentSettings{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return AgentSettings{}, ErrNotFound
	}
	return settings, nil
}

func (s *Store) ConversationExists(ctx context.Context, id string) (bool, error) {
	var value int
	err := s.ReadDB().QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=?`, id).Scan(&value)
	if errors.Is(err, sqlErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ListConversations(ctx context.Context, projectID *string) ([]map[string]any, error) {
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT c.id,c.project_id,c.title,c.memory_enabled,c.created_at,c.updated_at,COUNT(t.id) FROM conversations c LEFT JOIN conversation_turns t ON t.conversation_id=c.id WHERE (? IS NULL OR c.project_id=?) GROUP BY c.id ORDER BY c.updated_at DESC`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, project, title, createdAt, updatedAt string
		var enabled, count int
		if err := rows.Scan(&id, &project, &title, &enabled, &createdAt, &updatedAt, &count); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"id": id, "project_id": project, "title": title, "memory_enabled": enabled != 0, "created_at": createdAt, "updated_at": updatedAt, "turn_count": count})
	}
	return result, rows.Err()
}

func (s *Store) ListConversationsPage(ctx context.Context, projectID *string, limit, offset int) ([]map[string]any, int, error) {
	limit = max(1, limit)
	offset = max(0, offset)
	var total int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE (? IS NULL OR project_id=?)`, projectID, projectID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT c.id,c.project_id,c.title,c.memory_enabled,c.created_at,c.updated_at,COUNT(t.id) FROM conversations c LEFT JOIN conversation_turns t ON t.conversation_id=c.id WHERE (? IS NULL OR c.project_id=?) GROUP BY c.id ORDER BY c.updated_at DESC LIMIT ? OFFSET ?`, projectID, projectID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, project, title, createdAt, updatedAt string
		var enabled, count int
		if err := rows.Scan(&id, &project, &title, &enabled, &createdAt, &updatedAt, &count); err != nil {
			return nil, 0, err
		}
		result = append(result, map[string]any{"id": id, "project_id": project, "title": title, "memory_enabled": enabled != 0, "created_at": createdAt, "updated_at": updatedAt, "turn_count": count})
	}
	return result, total, rows.Err()
}

func (s *Store) SaveConversationTurn(ctx context.Context, conversationID, projectID, question, answer string, sources any, chatModel string, memoryEnabled bool, metrics any) (string, int64, error) {
	sourcesJSON, err := json.Marshal(sources)
	if err != nil {
		return "", 0, err
	}
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return "", 0, err
	}
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	now := utcNow()
	currentTurns := 0
	if conversationID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns WHERE conversation_id=? AND EXISTS(SELECT 1 FROM conversations WHERE id=?)`, conversationID, conversationID).Scan(&currentTurns); err != nil {
			return "", 0, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id=?`, conversationID).Scan(&exists); err != nil || exists == 0 {
			return "", 0, ErrNotFound
		}
	} else {
		if projectID == "" {
			projectID = DefaultProjectID
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, projectID).Scan(&exists); err != nil || exists == 0 {
			return "", 0, ErrNotFound
		}
		conversationID, err = newID()
		if err != nil {
			return "", 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,project_id,title,memory_enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`, conversationID, projectID, conversationTitle(question), boolInt(memoryEnabled), now, now); err != nil {
			return "", 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_turns(conversation_id,question,answer,sources_json,chat_model,agent_metrics_json,created_at) VALUES(?,?,?,?,?,?,?)`, conversationID, question, answer, string(sourcesJSON), chatModel, string(metricsJSON), now)
	if err != nil {
		return "", 0, err
	}
	turnID, err := result.LastInsertId()
	if err != nil {
		return "", 0, err
	}
	if currentTurns == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE conversations SET title=?,memory_enabled=?,updated_at=? WHERE id=?`, conversationTitle(question), boolInt(memoryEnabled), now, conversationID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE conversations SET memory_enabled=?,updated_at=? WHERE id=?`, boolInt(memoryEnabled), now, conversationID)
	}
	if err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET updated_at=? WHERE id=(SELECT project_id FROM conversations WHERE id=?)`, now, conversationID); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return conversationID, turnID, nil
}

func (s *Store) GetConversation(ctx context.Context, id string) (map[string]any, error) {
	var projectID, title, createdAt, updatedAt string
	var enabled int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT project_id,title,memory_enabled,created_at,updated_at FROM conversations WHERE id=?`, id).Scan(&projectID, &title, &enabled, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT id,question,answer,sources_json,chat_model,agent_metrics_json,created_at FROM conversation_turns WHERE conversation_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []map[string]any{}
	for rows.Next() {
		var turnID int64
		var question, answer, sourcesJSON, model, metricsJSON, at string
		if err := rows.Scan(&turnID, &question, &answer, &sourcesJSON, &model, &metricsJSON, &at); err != nil {
			return nil, err
		}
		var sources []map[string]any
		var metrics map[string]any
		_ = json.Unmarshal([]byte(sourcesJSON), &sources)
		_ = json.Unmarshal([]byte(metricsJSON), &metrics)
		if sources == nil {
			sources = []map[string]any{}
		}
		if metrics == nil {
			metrics = map[string]any{}
		}
		metrics = normalizeAgentMetrics(metrics)
		turns = append(turns, map[string]any{"id": turnID, "question": question, "answer": answer, "sources": sources, "chat_model": model, "agent_metrics": metrics, "created_at": at})
	}
	memory, err := s.ConversationMemoryStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "project_id": projectID, "title": title, "memory": memory, "created_at": createdAt, "updated_at": updatedAt, "turns": turns}, rows.Err()
}

func (s *Store) GetConversationPage(ctx context.Context, id string, beforeID int64, limit int) (map[string]any, error) {
	limit = max(1, limit)
	var projectID, title, createdAt, updatedAt string
	var enabled int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT project_id,title,memory_enabled,created_at,updated_at FROM conversations WHERE id=?`, id).Scan(&projectID, &title, &enabled, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var total int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns WHERE conversation_id=?`, id).Scan(&total); err != nil {
		return nil, err
	}
	query := `SELECT id,question,answer,sources_json,chat_model,agent_metrics_json,created_at FROM conversation_turns WHERE conversation_id=?`
	args := []any{id}
	if beforeID > 0 {
		query += ` AND id<?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.ReadDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []map[string]any{}
	for rows.Next() {
		var turnID int64
		var question, answer, sourcesJSON, model, metricsJSON, at string
		if err := rows.Scan(&turnID, &question, &answer, &sourcesJSON, &model, &metricsJSON, &at); err != nil {
			return nil, err
		}
		var sources []map[string]any
		var metrics map[string]any
		_ = json.Unmarshal([]byte(sourcesJSON), &sources)
		_ = json.Unmarshal([]byte(metricsJSON), &metrics)
		if sources == nil {
			sources = []map[string]any{}
		}
		if metrics == nil {
			metrics = map[string]any{}
		}
		turns = append(turns, map[string]any{"id": turnID, "question": question, "answer": answer, "sources": sources, "chat_model": model, "agent_metrics": normalizeAgentMetrics(metrics), "created_at": at})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
	nextBefore := int64(0)
	if len(turns) > 0 {
		nextBefore, _ = turns[0]["id"].(int64)
	}
	hasMore := false
	if nextBefore > 0 {
		var exists int
		if err := s.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_turns WHERE conversation_id=? AND id<?)`, id, nextBefore).Scan(&exists); err != nil {
			return nil, err
		}
		hasMore = exists != 0
	}
	memory, err := s.ConversationMemoryStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "project_id": projectID, "title": title, "memory": memory, "created_at": createdAt, "updated_at": updatedAt, "turns": turns, "turn_page": map[string]any{"limit": limit, "before_id": beforeID, "next_before_id": nextBefore, "has_more": hasMore, "total": total}}, nil
}

func (s *Store) ConversationProjectID(ctx context.Context, id string) (string, error) {
	var projectID string
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT project_id FROM conversations WHERE id=?`, id).Scan(&projectID); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return projectID, nil
}

func (s *Store) DeleteConversation(ctx context.Context, id string) (bool, error) {
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM conversations WHERE id=?`, id).Scan(&projectID); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id=?`, id); err != nil {
		return false, err
	}
	// El resumen del proyecto puede contener datos del turno eliminado. Se
	// invalida completo para que el siguiente ciclo lo reconstruya sólo con
	// las conversaciones que siguen existiendo.
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_memory WHERE project_id=?`, projectID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET updated_at=? WHERE id=?`, utcNow(), projectID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SetConversationMemoryEnabled(ctx context.Context, id string, enabled bool) (map[string]any, error) {
	result, err := s.WriteDB().ExecContext(ctx, `UPDATE conversations SET memory_enabled=?,updated_at=? WHERE id=?`, boolInt(enabled), utcNow(), id)
	if err != nil {
		return nil, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return nil, ErrNotFound
	}
	return s.ConversationMemoryStatus(ctx, id)
}

func (s *Store) ConversationMemoryStatus(ctx context.Context, id string) (map[string]any, error) {
	return s.memoryStatus(ctx, "conversation", id)
}
func (s *Store) ProjectMemoryStatus(ctx context.Context, id string) (map[string]any, error) {
	return s.memoryStatus(ctx, "project", id)
}

func (s *Store) memoryStatus(ctx context.Context, kind, id string) (map[string]any, error) {
	query := `SELECT c.memory_enabled,COUNT(t.id),COALESCE(m.summary,''),COALESCE(m.summarized_until_turn_id,0),COALESCE(m.summarized_turn_count,0),COALESCE(m.summary_model,''),m.last_error,m.updated_at FROM conversations c LEFT JOIN conversation_turns t ON t.conversation_id=c.id LEFT JOIN conversation_memory m ON m.conversation_id=c.id WHERE c.id=? GROUP BY c.id`
	if kind == "project" {
		query = `SELECT p.project_memory_enabled,COUNT(t.id),COALESCE(m.summary,''),COALESCE(m.summarized_until_turn_id,0),COALESCE(m.summarized_turn_count,0),COALESCE(m.summary_model,''),m.last_error,m.updated_at FROM projects p LEFT JOIN conversations c ON c.project_id=p.id LEFT JOIN conversation_turns t ON t.conversation_id=c.id LEFT JOIN project_memory m ON m.project_id=p.id WHERE p.id=? GROUP BY p.id`
	}
	var enabled, total, until, summarized int
	var summary, model string
	var lastError, updatedAt *string
	if err := s.ReadDB().QueryRowContext(ctx, query, id).Scan(&enabled, &total, &summary, &until, &summarized, &model, &lastError, &updatedAt); err != nil {
		if errors.Is(err, sqlErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return map[string]any{"enabled": enabled != 0, "has_summary": strings.TrimSpace(summary) != "", "summary": summary, "summarized_until_turn_id": until, "summarized_turns": summarized, "pending_turns": max(0, total-summarized), "total_turns": total, "summary_model": model, "last_error": lastError, "updated_at": updatedAt}, nil
}

func (s *Store) LoadConversationMemory(ctx context.Context, id string, recentLimit int) (map[string]any, error) {
	status, err := s.ConversationMemoryStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.ReadDB().QueryContext(ctx, `SELECT id,question,answer FROM conversation_turns WHERE conversation_id=? ORDER BY id DESC LIMIT ?`, id, recentLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []map[string]any{}
	for rows.Next() {
		var turnID int64
		var question, answer string
		if err := rows.Scan(&turnID, &question, &answer); err != nil {
			return nil, err
		}
		turns = append(turns, map[string]any{"id": turnID, "question": question, "answer": answer})
	}
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
	return map[string]any{"enabled": status["enabled"], "summary": status["summary"], "turns": turns}, rows.Err()
}

func (s *Store) SaveMemorySummary(ctx context.Context, kind, id, summary string, until, count int, model string) (bool, error) {
	table, key := "conversation_memory", "conversation_id"
	if kind == "project" {
		table, key = "project_memory", "project_id"
	}
	tx, err := s.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var current int
	err = tx.QueryRowContext(ctx, `SELECT summarized_until_turn_id FROM `+table+` WHERE `+key+`=?`, id).Scan(&current)
	if err != nil && !errors.Is(err, sqlErrNoRows) {
		return false, err
	}
	if until <= current {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(`+key+`,summary,summarized_until_turn_id,summarized_turn_count,summary_model,last_error,updated_at) VALUES(?,?,?,?,?,NULL,?) ON CONFLICT(`+key+`) DO UPDATE SET summary=excluded.summary,summarized_until_turn_id=excluded.summarized_until_turn_id,summarized_turn_count=excluded.summarized_turn_count,summary_model=excluded.summary_model,last_error=NULL,updated_at=excluded.updated_at`, id, summary, until, count, model, utcNow())
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SetMemoryError(ctx context.Context, kind, id, message string) error {
	table, key := "conversation_memory", "conversation_id"
	if kind == "project" {
		table, key = "project_memory", "project_id"
	}
	_, err := s.WriteDB().ExecContext(ctx, `INSERT INTO `+table+`(`+key+`,last_error,updated_at) VALUES(?,?,?) ON CONFLICT(`+key+`) DO UPDATE SET last_error=excluded.last_error,updated_at=excluded.updated_at`, id, message, utcNow())
	return err
}

func (s *Store) UnsummarizedTurns(ctx context.Context, kind, id string, batchSize int) ([]map[string]any, string, int, error) {
	table, key := "conversation_memory", "conversation_id"
	query := `SELECT t.id,'',t.question,t.answer FROM conversation_turns t WHERE t.conversation_id=? AND t.id>? ORDER BY t.id LIMIT ?`
	if kind == "project" {
		table, key = "project_memory", "project_id"
		query = `SELECT t.id,c.title,t.question,t.answer FROM conversation_turns t JOIN conversations c ON c.id=t.conversation_id WHERE c.project_id=? AND t.id>? ORDER BY t.id LIMIT ?`
	}
	var summary string
	var until, count int
	err := s.ReadDB().QueryRowContext(ctx, `SELECT summary,summarized_until_turn_id,summarized_turn_count FROM `+table+` WHERE `+key+`=?`, id).Scan(&summary, &until, &count)
	if errors.Is(err, sqlErrNoRows) {
		err = nil
	}
	if err != nil {
		return nil, "", 0, err
	}
	rows, err := s.ReadDB().QueryContext(ctx, query, id, until, batchSize)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()
	turns := []map[string]any{}
	for rows.Next() {
		var turnID int
		var title, question, answer string
		if err := rows.Scan(&turnID, &title, &question, &answer); err != nil {
			return nil, "", 0, err
		}
		turns = append(turns, map[string]any{"id": turnID, "conversation_title": title, "question": question, "answer": answer})
	}
	return turns, summary, count, rows.Err()
}

func normalizeName(value string) string { return strings.Join(strings.Fields(value), " ") }
func conversationTitle(question string) string {
	value := normalizeName(question)
	if value == "" {
		return "Nueva conversación"
	}
	runes := []rune(value)
	if len(runes) <= 72 {
		return value
	}
	return string(runes[:71]) + "…"
}
func emptyContextUsage() map[string]any {
	role := func() map[string]any {
		return map[string]any{"latest": nil, "maximum_percent": nil, "average_percent": nil, "samples": 0}
	}
	return map[string]any{"verifier": role(), "writer": role(), "latest_percent": nil, "maximum_percent": nil, "average_percent": nil}
}

func normalizeAgentMetrics(metrics map[string]any) map[string]any {
	if len(metrics) == 0 {
		return metrics
	}
	required := []string{"model", "model_context_window", "configured_context_window", "effective_context_window", "context_source"}
	for _, role := range []string{"verifier", "writer"} {
		value, ok := metrics[role].(map[string]any)
		if !ok || len(value) == 0 {
			continue
		}
		incomplete := false
		for _, field := range required {
			if _, exists := value[field]; !exists {
				incomplete = true
				break
			}
		}
		value["incomplete_context_data"] = incomplete
		metrics[role] = value
	}
	return metrics
}
