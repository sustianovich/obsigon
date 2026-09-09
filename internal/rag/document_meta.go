package rag

import (
	"fmt"
	"strings"
	"time"
)

var (
	validFromKeys  = []string{"fecha_vigencia", "vigente_desde", "fecha_inicio", "valid_from", "effective_date"}
	validUntilKeys = []string{"fecha_derogacion", "fecha_derogación", "derogado_el", "fecha_fin", "valid_until", "expiry_date"}
	reviewKeys     = []string{"fecha_revision", "fecha_revisión", "revisar_el", "review_date"}
)

type Vigencia struct {
	TemporalStatus  string  `json:"estado_temporal"`
	ValidFrom       *string `json:"valid_from"`
	ValidUntil      *string `json:"valid_until"`
	ReviewDate      *string `json:"review_date"`
	OverdueReview   bool    `json:"revision_vencida"`
	DeprecatedAlert bool    `json:"derogado_por_callout"`
}

func ParseDate(value any) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	if parsed, ok := value.(time.Time); ok {
		return dateOnly(parsed), true
	}
	text := strings.TrimSpace(stringify(value))
	if text == "" || text == "true" || text == "false" {
		return time.Time{}, false
	}
	text = strings.ReplaceAll(text, "/", "-")
	for _, layout := range []string{"2006-01-02", "02-01-2006", "2006-01", "2006", time.RFC3339, "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return dateOnly(parsed), true
		}
	}
	return time.Time{}, false
}

func firstDate(metadata map[string]any, keys []string) (time.Time, bool) {
	for _, key := range keys {
		if value, exists := metadata[key]; exists {
			if parsed, ok := ParseDate(value); ok {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func ComputeVigencia(metadata map[string]any, deprecated bool, today time.Time) Vigencia {
	if today.IsZero() {
		today = time.Now()
	}
	reference := dateOnly(today)
	from, hasFrom := firstDate(metadata, validFromKeys)
	until, hasUntil := firstDate(metadata, validUntilKeys)
	review, hasReview := firstDate(metadata, reviewKeys)
	status := "desconocida"
	if hasUntil && !until.After(reference) {
		status = "caducada"
	} else if hasFrom && from.After(reference) {
		status = "futura"
	} else if hasFrom || hasUntil {
		status = "vigente"
	}
	if deprecated && status != "futura" {
		status = "caducada"
	}
	return Vigencia{TemporalStatus: status, ValidFrom: dateString(from, hasFrom), ValidUntil: dateString(until, hasUntil), ReviewDate: dateString(review, hasReview), OverdueReview: hasReview && !review.After(reference), DeprecatedAlert: deprecated}
}

func BuildEnrichment(metadata map[string]any, tags []string, vigencia Vigencia, anchors map[string]string, callouts []Callout) map[string]any {
	result := make(map[string]any, len(metadata)+4)
	for key, value := range metadata {
		result[key] = value
	}
	result["_tags"] = tags
	result["_vigencia"] = vigencia
	result["_anclas"] = anchors
	result["_callouts"] = callouts
	return result
}

func dateOnly(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}
func dateString(value time.Time, ok bool) *string {
	if !ok {
		return nil
	}
	text := value.Format("2006-01-02")
	return &text
}
func stringify(value any) string { return fmt.Sprint(value) }
