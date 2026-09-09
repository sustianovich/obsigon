package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeLocalOllamaURL(t *testing.T) {
	valid := []string{"http://127.0.0.1:11434", "http://localhost:11434/", "https://[::1]:11434"}
	for _, value := range valid {
		if _, err := NormalizeLocalOllamaURL(value); err != nil {
			t.Fatalf("URL local rechazada %q: %v", value, err)
		}
	}
	invalid := []string{"http://example.com:11434", "http://127.0.0.1:11434/api", "http://user@localhost:11434"}
	for _, value := range invalid {
		if _, err := NormalizeLocalOllamaURL(value); err == nil {
			t.Fatalf("URL no local aceptada: %q", value)
		}
	}
}

func TestNormalizeLoopbackHost(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "localhost", "::1", "[::1]"} {
		if _, err := NormalizeLoopbackHost(value); err != nil {
			t.Fatalf("host local rechazado %q: %v", value, err)
		}
	}
	for _, value := range []string{"0.0.0.0", "192.168.1.10", "example.com", "127.0.0.1:8000"} {
		if _, err := NormalizeLoopbackHost(value); err == nil {
			t.Fatalf("host no permitido aceptado: %q", value)
		}
	}
}

func TestLoadConfiguresHostPortAndWebUIOverride(t *testing.T) {
	base := t.TempDir()
	t.Setenv("APP_HOST", "localhost")
	t.Setenv("APP_PORT", "8123")
	t.Setenv("RAG_WEBUI_DIR", "frontend")
	settings, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AppHost != "localhost" || settings.AppPort != 8123 || settings.WebUIDir != filepath.Join(base, "frontend") {
		t.Fatalf("settings=%+v", settings)
	}
	t.Setenv("APP_PORT", "70000")
	if _, err := Load(base); err == nil {
		t.Fatal("se aceptó un puerto fuera de rango")
	}
}

func TestRejectCloudModel(t *testing.T) {
	if _, err := ValidateLocalModelName("modelo-cloud:latest", "OLLAMA_CHAT_MODEL"); err == nil {
		t.Fatal("se aceptó un modelo cloud")
	}
}

func TestSlugIsStableAndDistinct(t *testing.T) {
	a := filepath.Join(t.TempDir(), "uno", "Mi Vault")
	b := filepath.Join(t.TempDir(), "dos", "Mi Vault")
	if SlugifyVaultPath(a) != SlugifyVaultPath(a) {
		t.Fatal("el slug no es estable")
	}
	if SlugifyVaultPath(a) == SlugifyVaultPath(b) {
		t.Fatal("dos rutas distintas comparten slug")
	}
	if !strings.HasPrefix(SlugifyVaultPath(a), "mi-vault-") {
		t.Fatalf("slug inesperado: %s", SlugifyVaultPath(a))
	}
}

func TestInvalidBooleanIsRejected(t *testing.T) {
	t.Setenv("RAG_STREAMING", "quizá")
	if err := validateEnvironmentTypes(); err == nil {
		t.Fatal("se aceptó un booleano no válido")
	}
	_ = os.Unsetenv("RAG_STREAMING")
}

func TestAgenticRetrievalConfigurationIsOptInAndBounded(t *testing.T) {
	base := t.TempDir()
	settings, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentMode || settings.AgentMaxIterations != 4 || settings.AgentTokenBudget != 2200 || settings.AgentMaxSubquestions != 3 {
		t.Fatalf("defaults=%+v", settings)
	}
	t.Setenv("RAG_AGENT_MODE", "true")
	t.Setenv("RAG_AGENT_MAX_ITERATIONS", "99")
	t.Setenv("RAG_AGENT_TOKEN_BUDGET", "10")
	t.Setenv("RAG_AGENT_MAX_SUBQUESTIONS", "99")
	settings, err = Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.AgentMode || settings.AgentMaxIterations != 5 || settings.AgentTokenBudget != 600 || settings.AgentMaxSubquestions != 5 {
		t.Fatalf("bounded=%+v", settings)
	}
}

func TestPhaseOneRetrievalDefaults(t *testing.T) {
	settings, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !settings.QueryRewriteEnabled || settings.QueryRewriteContextTokens != 4096 || settings.QueryRewriteMaxOutputTokens != 512 || !settings.ReadLargeEnabled || !settings.HubBoostEnabled || settings.HubBoostWeight != .08 || !settings.RecencyBoostEnabled || settings.RecencyBoostWeight != .08 {
		t.Fatalf("settings=%+v", settings)
	}
}

func TestANNConfigurationIsOptInAndBounded(t *testing.T) {
	base := t.TempDir()
	settings, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ANNEnabled || settings.ANNM != 16 || settings.ANNEFConstruction != 200 || settings.ANNEFSearch != 64 {
		t.Fatalf("defaults ANN=%+v", settings)
	}
	t.Setenv("RAG_ANN", "true")
	t.Setenv("RAG_ANN_M", "999")
	t.Setenv("RAG_ANN_EF_CONSTRUCTION", "1")
	t.Setenv("RAG_ANN_EF_SEARCH", "9999")
	settings, err = Load(base)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ANNEnabled || settings.ANNM != 64 || settings.ANNEFConstruction != 16 || settings.ANNEFSearch != 2000 {
		t.Fatalf("límites ANN=%+v", settings)
	}
}

func TestOperationalDefaultsAndLocalCrossEncoder(t *testing.T) {
	settings, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if settings.WatchReconcileIntervalMS != 300000 || settings.HistoryPageSize != 50 || settings.HTTPReadHeaderTimeoutSeconds != 5 {
		t.Fatalf("settings=%+v", settings)
	}
	t.Setenv("RAG_RERANK", "true")
	t.Setenv("RAG_RERANK_BACKEND", "cross_encoder")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("cross_encoder activo sin URL")
	}
	t.Setenv("RAG_CROSS_ENCODER_URL", "http://example.com:9000")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("cross_encoder remoto aceptado")
	}
	t.Setenv("RAG_CROSS_ENCODER_URL", "http://127.0.0.1:9000")
	settings, err = Load(t.TempDir())
	if err != nil || settings.RerankBackend != "cross_encoder" {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
}
