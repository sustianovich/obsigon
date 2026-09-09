package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureMetadataDetectsDatasetVaultAndConfigurationChanges(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(vault, "note.md")
	if err := os.WriteFile(note, []byte("# Nota\n\nContenido estable.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	datasetRaw := []byte(`{"cases":[{"id":"one","question":"test"}]}`)
	settings := fixtureSettings()
	settings.VaultPath = vault
	data := newFixtureData("questions.json")
	data.Dimension = 2
	data.Embed["batch"] = [][]float32{{1, 0}}
	for _, model := range []string{settings.ChatModel, settings.EmbeddingModel} {
		raw, err := json.Marshal(map[string]any{"model": model, "digest": "stable-" + model})
		if err != nil {
			t.Fatal(err)
		}
		data.Model["modelshow\x00"+model] = raw
	}
	if err := finalizeFixtureData(data, "questions.json", datasetRaw, vault, settings); err != nil {
		t.Fatal(err)
	}
	if err := validateFixtureData(data, "questions.json", datasetRaw, vault, settings); err != nil {
		t.Fatalf("fixture recién creado inválido: %v", err)
	}

	changedSettings := settings
	changedSettings.TopK++
	if err := validateFixtureData(data, "questions.json", datasetRaw, vault, changedSettings); err == nil || !strings.Contains(err.Error(), "configuración") {
		t.Fatalf("se esperaba detectar configuración distinta, got %v", err)
	}
	if err := validateFixtureData(data, "questions.json", []byte(`{"cases":[]}`), vault, settings); err == nil || !strings.Contains(err.Error(), "dataset") {
		t.Fatalf("se esperaba detectar dataset distinto, got %v", err)
	}
	if err := os.WriteFile(note, []byte("# Nota\n\nContenido cambiado.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateFixtureData(data, "questions.json", datasetRaw, vault, settings); err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("se esperaba detectar vault distinto, got %v", err)
	}
}

func TestFixtureMetadataRejectsIncompleteModelIdentity(t *testing.T) {
	vault := t.TempDir()
	datasetRaw := []byte(`{"cases":[]}`)
	settings := fixtureSettings()
	settings.VaultPath = vault
	data := newFixtureData("questions.json")
	data.Dimension = 2
	data.Embed["batch"] = [][]float32{{1, 0}}
	if err := finalizeFixtureData(data, "questions.json", datasetRaw, vault, settings); err == nil || !strings.Contains(err.Error(), "metadatos del modelo") {
		t.Fatalf("se esperaba detectar identidad ausente, got %v", err)
	}
}

func TestRequireSemanticBackend(t *testing.T) {
	report := evaluationReport{Results: []caseResult{{ID: "one", SemanticBackend: "ann"}}}
	if err := requireSemanticBackend(report, "ann"); err != nil {
		t.Fatal(err)
	}
	if err := requireSemanticBackend(report, "exact"); err == nil {
		t.Fatal("se esperaba rechazo al degradar ANN a exact")
	}
}
