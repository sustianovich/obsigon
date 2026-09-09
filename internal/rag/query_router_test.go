package rag

import (
	"context"
	"slices"
	"testing"
)

func TestQueryRouterUsesAuditableSignalsWithoutMutatingSettings(t *testing.T) {
	store, _, settings := retrievalFixture(t)
	settings.QueryRouterRelationalPatterns = []string{"relación entre"}
	settings.QueryRouterStructuralTerms = []string{"relación", "flujo"}
	settings.QueryRouterStructuralThreshold = 2
	settings.GraphMaxHops = 2
	settings.GraphWeight = .5
	policy, err := RouteQuery(context.Background(), "¿Qué relación entre Protocolo validación y Borrador alternativo existe?", nil, settings, store)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Class != QueryRelational || !policy.GraphEnabled || settings.GraphSearchEnabled {
		t.Fatalf("policy=%+v settings=%+v", policy, settings)
	}
	if !slices.Contains(policy.Reasons, "notas_mencionadas:2") {
		t.Fatalf("razones=%v", policy.Reasons)
	}

	factual, err := RouteQuery(context.Background(), "¿Cuál es el criterio de validación?", nil, settings, store)
	if err != nil {
		t.Fatal(err)
	}
	if factual.Class != QueryFactual || factual.GraphEnabled {
		t.Fatalf("policy factual=%+v", factual)
	}
}
