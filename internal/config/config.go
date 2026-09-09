// Package config carga la configuración local y aplica las barreras de
// privacidad antes de que arranque ningún cliente.
package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultOllamaURL    = "http://127.0.0.1:11434"
	DefaultChatModel    = "gpt-oss:latest"
	DefaultEmbedModel   = "nomic-embed-text"
	DefaultAppHost      = "127.0.0.1"
	DefaultAppPort      = 8000
	DefaultChunkSize    = 1800
	DefaultChunkOverlap = 250
)

type Settings struct {
	BaseDir                           string
	AppHost                           string
	AppPort                           int
	HTTPReadHeaderTimeoutSeconds      int
	HTTPIdleTimeoutSeconds            int
	HTTPShutdownTimeoutSeconds        int
	WebUIDir                          string
	OllamaBaseURL                     string
	ChatModel                         string
	EmbeddingModel                    string
	OllamaTimeoutSeconds              float64
	OllamaKeepAlive                   string
	VaultPath                         string
	DatabasePath                      string
	DatabasePathExplicit              bool
	ChunkSize                         int
	ChunkOverlap                      int
	ParentChildEnabled                bool
	ParentChunkSize                   int
	ChildChunkSize                    int
	ChildChunkOverlap                 int
	EmbeddingPrefixScheme             string
	EmbeddingWorkers                  int
	ANNEnabled                        bool
	ANNM                              int
	ANNEFConstruction                 int
	ANNEFSearch                       int
	TopK                              int
	MinSimilarity                     float64
	MinRelativeScore                  float64
	MaxOutputTokens                   int
	HybridSearchEnabled               bool
	HybridRRFK                        int
	HybridSemanticWeight              float64
	HybridLexicalWeight               float64
	HybridCandidateMultiplier         int
	MMREnabled                        bool
	MMRLambda                         float64
	MaxChunksPerDocument              int
	GraphSearchEnabled                bool
	GraphWeight                       float64
	GraphMaxHops                      int
	GraphDecay                        float64
	GraphBacklinkWeight               float64
	GraphSeedDocuments                int
	GraphMaxCandidates                int
	RerankEnabled                     bool
	RerankBackend                     string
	RerankCandidates                  int
	RerankWeight                      float64
	CrossEncoderURL                   string
	CrossEncoderModel                 string
	CrossEncoderTimeoutSeconds        float64
	QueryRoutingEnabled               bool
	QueryRouterRelationalPatterns     []string
	QueryRouterStructuralTerms        []string
	QueryRouterStructuralThreshold    int
	QueryRewriteEnabled               bool
	QueryRewriteContextTokens         int
	QueryRewriteMaxOutputTokens       int
	QueryRewriteMaxMemoryChars        int
	ReadLargeEnabled                  bool
	HubBoostEnabled                   bool
	HubBoostWeight                    float64
	RecencyBoostEnabled               bool
	RecencyBoostWeight                float64
	WatchEnabled                      bool
	WatchReconcileIntervalMS          int
	WatchDebounceMS                   int
	HistoryPageSize                   int
	HistoryMaxPageSize                int
	StreamEnabled                     bool
	AgentMode                         bool
	AgentMaxIterations                int
	AgentTokenBudget                  int
	AgentMaxSubquestions              int
	AgentContextTokens                int
	AgentMaxObservationChars          int
	VerifierContextTokens             int
	WriterContextTokens               int
	VerifierMaxOutputTokens           int
	VerifierAbstainOnInsufficient     bool
	MemorySummaryInterval             int
	MemoryRecentTurns                 int
	MemoryMaxContextChars             int
	MemorySummaryMaxInputChars        int
	MemorySummaryMaxTokens            int
	ProjectMemoryMaxContextChars      int
	ProjectMemorySummaryInterval      int
	ProjectMemorySummaryMaxInputChars int
	ProjectMemorySummaryMaxTokens     int
}

func Load(baseDir string) (Settings, error) {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return Settings{}, fmt.Errorf("no se pudo resolver la raíz del proyecto: %w", err)
	}
	if err := loadDotEnv(filepath.Join(abs, ".env")); err != nil {
		return Settings{}, err
	}
	if err := validateEnvironmentTypes(); err != nil {
		return Settings{}, err
	}
	appHost, err := NormalizeLoopbackHost(env("APP_HOST", DefaultAppHost))
	if err != nil {
		return Settings{}, err
	}
	appPort := envInt("APP_PORT", DefaultAppPort)
	if appPort < 1 || appPort > 65535 {
		return Settings{}, errors.New("APP_PORT debe estar entre 1 y 65535")
	}
	webUIDir := ""
	if raw := strings.TrimSpace(os.Getenv("RAG_WEBUI_DIR")); raw != "" {
		webUIDir, err = resolvePath(abs, raw)
		if err != nil {
			return Settings{}, fmt.Errorf("RAG_WEBUI_DIR no es válida: %w", err)
		}
	}

	ollamaURL, err := NormalizeLocalOllamaURL(env("OLLAMA_BASE_URL", DefaultOllamaURL))
	if err != nil {
		return Settings{}, err
	}
	crossEncoderURL := ""
	if raw := strings.TrimSpace(os.Getenv("RAG_CROSS_ENCODER_URL")); raw != "" {
		crossEncoderURL, err = normalizeLocalServiceURL(raw, "RAG_CROSS_ENCODER_URL")
		if err != nil {
			return Settings{}, err
		}
	}
	chat, err := ValidateLocalModelName(env("OLLAMA_CHAT_MODEL", DefaultChatModel), "OLLAMA_CHAT_MODEL")
	if err != nil {
		return Settings{}, err
	}
	embed, err := ValidateLocalModelName(env("OLLAMA_EMBEDDING_MODEL", DefaultEmbedModel), "OLLAMA_EMBEDDING_MODEL")
	if err != nil {
		return Settings{}, err
	}

	vault, err := resolvePath(abs, env("OBSIDIAN_VAULT_PATH", "./vault_demo"))
	if err != nil {
		return Settings{}, fmt.Errorf("OBSIDIAN_VAULT_PATH no es válida: %w", err)
	}
	rawDB := strings.TrimSpace(os.Getenv("RAG_DATABASE_PATH"))
	dbExplicit := rawDB != ""
	var database string
	if dbExplicit {
		database, err = resolvePath(abs, rawDB)
	} else {
		database = DefaultDatabasePath(abs, vault)
	}
	if err != nil {
		return Settings{}, fmt.Errorf("RAG_DATABASE_PATH no es válida: %w", err)
	}

	parentSize := clampInt(envInt("RAG_PARENT_CHUNK_SIZE", 6000), 1000, 20000)
	childSize := min(parentSize, clampInt(envInt("RAG_CHILD_CHUNK_SIZE", 700), 200, 4000))
	childOverlap := min(childSize/2, max(0, envInt("RAG_CHILD_CHUNK_OVERLAP", 100)))

	settings := Settings{
		BaseDir: abs, AppHost: appHost, AppPort: appPort, WebUIDir: webUIDir,
		HTTPReadHeaderTimeoutSeconds: clampInt(envInt("APP_READ_HEADER_TIMEOUT_SECONDS", 5), 1, 60),
		HTTPIdleTimeoutSeconds:       clampInt(envInt("APP_IDLE_TIMEOUT_SECONDS", 90), 10, 600),
		HTTPShutdownTimeoutSeconds:   clampInt(envInt("APP_SHUTDOWN_TIMEOUT_SECONDS", 10), 1, 120),
		OllamaBaseURL:                ollamaURL, ChatModel: chat,
		EmbeddingModel:       embed,
		OllamaTimeoutSeconds: maxFloat(10, envFloat("OLLAMA_TIMEOUT_SECONDS", 180)),
		OllamaKeepAlive:      nonEmpty(env("OLLAMA_KEEP_ALIVE", "30m"), "30m"),
		VaultPath:            vault, DatabasePath: database, DatabasePathExplicit: dbExplicit,
		ChunkSize:          max(500, envInt("RAG_CHUNK_SIZE", DefaultChunkSize)),
		ChunkOverlap:       max(0, envInt("RAG_CHUNK_OVERLAP", DefaultChunkOverlap)),
		ParentChildEnabled: envBool("RAG_PARENT_CHILD_CHUNKING", false),
		ParentChunkSize:    parentSize, ChildChunkSize: childSize, ChildChunkOverlap: childOverlap,
		EmbeddingPrefixScheme:     nonEmpty(env("RAG_EMBEDDING_PREFIX_SCHEME", "auto"), "auto"),
		EmbeddingWorkers:          clampInt(envInt("RAG_EMBEDDING_WORKERS", 3), 1, 8),
		ANNEnabled:                envBool("RAG_ANN", false),
		ANNM:                      clampInt(envInt("RAG_ANN_M", 16), 4, 64),
		ANNEFConstruction:         clampInt(envInt("RAG_ANN_EF_CONSTRUCTION", 200), 16, 2000),
		ANNEFSearch:               clampInt(envInt("RAG_ANN_EF_SEARCH", 64), 8, 2000),
		TopK:                      max(1, envInt("RAG_TOP_K", 6)),
		MinSimilarity:             clampFloat(envFloat("RAG_MIN_SIMILARITY", .30), 0, 1),
		MinRelativeScore:          clampFloat(envFloat("RAG_MIN_RELATIVE_SCORE", .62), 0, .99),
		MaxOutputTokens:           max(300, envInt("RAG_MAX_OUTPUT_TOKENS", 1800)),
		HybridSearchEnabled:       envBool("RAG_HYBRID_SEARCH", true),
		HybridRRFK:                clampInt(envInt("RAG_HYBRID_RRF_K", 60), 1, 200),
		HybridSemanticWeight:      maxFloat(0, envFloat("RAG_HYBRID_SEMANTIC_WEIGHT", 1)),
		HybridLexicalWeight:       maxFloat(0, envFloat("RAG_HYBRID_LEXICAL_WEIGHT", .8)),
		HybridCandidateMultiplier: clampInt(envInt("RAG_HYBRID_CANDIDATE_MULTIPLIER", 4), 2, 20),
		MMREnabled:                envBool("RAG_MMR", true), MMRLambda: clampFloat(envFloat("RAG_MMR_LAMBDA", .72), 0, 1),
		MaxChunksPerDocument:              clampInt(envInt("RAG_MAX_CHUNKS_PER_DOCUMENT", 3), 1, 10),
		GraphSearchEnabled:                envBool("RAG_GRAPH_SEARCH", false),
		GraphWeight:                       maxFloat(0, envFloat("RAG_GRAPH_WEIGHT", .5)),
		GraphMaxHops:                      clampInt(envInt("RAG_GRAPH_MAX_HOPS", 2), 1, 5),
		GraphDecay:                        clampFloat(envFloat("RAG_GRAPH_DECAY", .5), 0, 1),
		GraphBacklinkWeight:               clampFloat(envFloat("RAG_GRAPH_BACKLINK_WEIGHT", .7), 0, 1),
		GraphSeedDocuments:                clampInt(envInt("RAG_GRAPH_SEED_DOCUMENTS", 4), 1, 50),
		GraphMaxCandidates:                clampInt(envInt("RAG_GRAPH_MAX_CANDIDATES", 20), 1, 100),
		RerankEnabled:                     envBool("RAG_RERANK", false),
		RerankBackend:                     strings.ToLower(nonEmpty(env("RAG_RERANK_BACKEND", "llm"), "llm")),
		RerankCandidates:                  clampInt(envInt("RAG_RERANK_CANDIDATES", 12), 1, 50),
		RerankWeight:                      clampFloat(envFloat("RAG_RERANK_WEIGHT", .35), 0, 1),
		CrossEncoderURL:                   crossEncoderURL,
		CrossEncoderModel:                 nonEmpty(env("RAG_CROSS_ENCODER_MODEL", "cross-encoder/ms-marco-MiniLM-L-6-v2"), "cross-encoder/ms-marco-MiniLM-L-6-v2"),
		CrossEncoderTimeoutSeconds:        maxFloat(1, envFloat("RAG_CROSS_ENCODER_TIMEOUT_SECONDS", 30)),
		QueryRoutingEnabled:               envBool("RAG_QUERY_ROUTING", false),
		QueryRouterRelationalPatterns:     envList("RAG_QUERY_ROUTER_RELATIONAL_PATTERNS", "relación entre,relacion entre,cómo se conecta,como se conecta,depende de,flujo entre,antes y después,antes y despues"),
		QueryRouterStructuralTerms:        envList("RAG_QUERY_ROUTER_STRUCTURAL_TERMS", "relación,relacion,dependencia,flujo,proceso,paso,enlace,conexión,conexion,ruta,circuito"),
		QueryRouterStructuralThreshold:    clampInt(envInt("RAG_QUERY_ROUTER_STRUCTURAL_THRESHOLD", 2), 1, 10),
		QueryRewriteEnabled:               envBool("RAG_QUERY_REWRITE", true),
		QueryRewriteContextTokens:         clampInt(envInt("RAG_QUERY_REWRITE_CONTEXT_TOKENS", 4096), 2048, 32768),
		QueryRewriteMaxOutputTokens:       clampInt(envInt("RAG_QUERY_REWRITE_MAX_OUTPUT_TOKENS", 512), 64, 1024),
		QueryRewriteMaxMemoryChars:        clampInt(envInt("RAG_QUERY_REWRITE_MAX_MEMORY_CHARS", 6000), 1000, 24000),
		ReadLargeEnabled:                  envBool("RAG_READ_LARGE", true),
		HubBoostEnabled:                   envBool("RAG_HUB_BOOST", true),
		HubBoostWeight:                    clampFloat(envFloat("RAG_HUB_BOOST_WEIGHT", .08), 0, .5),
		RecencyBoostEnabled:               envBool("RAG_RECENCY_BOOST", true),
		RecencyBoostWeight:                clampFloat(envFloat("RAG_RECENCY_BOOST_WEIGHT", .08), 0, .5),
		WatchEnabled:                      envBool("RAG_WATCH", false),
		WatchReconcileIntervalMS:          clampInt(envInt("RAG_WATCH_RECONCILE_INTERVAL_MS", 300000), 30000, 3600000),
		WatchDebounceMS:                   clampInt(envInt("RAG_WATCH_DEBOUNCE_MS", 750), 100, 10000),
		HistoryPageSize:                   clampInt(envInt("RAG_HISTORY_PAGE_SIZE", 50), 10, 200),
		HistoryMaxPageSize:                clampInt(envInt("RAG_HISTORY_MAX_PAGE_SIZE", 200), 50, 500),
		StreamEnabled:                     envBool("RAG_STREAMING", true),
		AgentMode:                         envBool("RAG_AGENT_MODE", false),
		AgentMaxIterations:                clampInt(envInt("RAG_AGENT_MAX_ITERATIONS", 4), 3, 5),
		AgentTokenBudget:                  clampInt(envInt("RAG_AGENT_TOKEN_BUDGET", 2200), 600, 32000),
		AgentMaxSubquestions:              clampInt(envInt("RAG_AGENT_MAX_SUBQUESTIONS", 3), 1, 5),
		AgentContextTokens:                clampInt(envInt("RAG_AGENT_CONTEXT_TOKENS", 8192), 4096, 262144),
		AgentMaxObservationChars:          clampInt(envInt("RAG_AGENT_MAX_OBSERVATION_CHARS", 12000), 1000, 100000),
		VerifierContextTokens:             clampInt(envInt("RAG_VERIFIER_CONTEXT_TOKENS", 8192), 4096, 262144),
		WriterContextTokens:               clampInt(envInt("RAG_WRITER_CONTEXT_TOKENS", 16384), 4096, 262144),
		VerifierMaxOutputTokens:           clampInt(envInt("RAG_VERIFIER_MAX_OUTPUT_TOKENS", 600), 200, 2000),
		VerifierAbstainOnInsufficient:     envBool("RAG_VERIFIER_ABSTAIN_ON_INSUFFICIENT", false),
		MemorySummaryInterval:             clampInt(envInt("RAG_MEMORY_SUMMARY_INTERVAL", 10), 2, 100),
		MemoryRecentTurns:                 clampInt(envInt("RAG_MEMORY_RECENT_TURNS", 4), 1, 20),
		MemoryMaxContextChars:             clampInt(envInt("RAG_MEMORY_MAX_CONTEXT_CHARS", 12000), 2000, 100000),
		MemorySummaryMaxInputChars:        clampInt(envInt("RAG_MEMORY_SUMMARY_MAX_INPUT_CHARS", 40000), 4000, 200000),
		MemorySummaryMaxTokens:            clampInt(envInt("RAG_MEMORY_SUMMARY_MAX_TOKENS", 700), 100, 4000),
		ProjectMemoryMaxContextChars:      clampInt(envInt("RAG_PROJECT_MEMORY_MAX_CONTEXT_CHARS", 6000), 1000, 50000),
		ProjectMemorySummaryInterval:      clampInt(envInt("RAG_PROJECT_MEMORY_SUMMARY_INTERVAL", 10), 2, 100),
		ProjectMemorySummaryMaxInputChars: clampInt(envInt("RAG_PROJECT_MEMORY_SUMMARY_MAX_INPUT_CHARS", 40000), 4000, 200000),
		ProjectMemorySummaryMaxTokens:     clampInt(envInt("RAG_PROJECT_MEMORY_SUMMARY_MAX_TOKENS", 700), 100, 4000),
	}
	if settings.RerankBackend != "llm" && settings.RerankBackend != "cross_encoder" && settings.RerankBackend != "noop" {
		return Settings{}, errors.New("RAG_RERANK_BACKEND debe ser llm, cross_encoder o noop")
	}
	if settings.RerankBackend == "cross_encoder" && settings.RerankEnabled && settings.CrossEncoderURL == "" {
		return Settings{}, errors.New("RAG_CROSS_ENCODER_URL es obligatoria al activar el backend cross_encoder")
	}
	return settings, nil
}

// NormalizeLoopbackHost aplica al servidor la misma barrera local que al
// cliente de Ollama. El puerto se configura por separado mediante APP_PORT.
func NormalizeLoopbackHost(value string) (string, error) {
	host := strings.TrimSpace(value)
	if host == "" {
		host = DefaultAppHost
	}
	bareHost := strings.Trim(host, "[]")
	if strings.ContainsAny(host, "/\\@?#") || strings.Contains(bareHost, ":") && net.ParseIP(bareHost) == nil {
		return "", errors.New("APP_HOST debe ser localhost o una dirección IP de loopback sin puerto")
	}
	normalized := strings.ToLower(bareHost)
	if normalized == "localhost" {
		return normalized, nil
	}
	ip := net.ParseIP(normalized)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("APP_HOST sólo puede ser localhost o una dirección IP de loopback")
	}
	return ip.String(), nil
}

func NormalizeLocalOllamaURL(value string) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(value), "/")
	if raw == "" {
		raw = DefaultOllamaURL
	}
	return normalizeLocalServiceURL(raw, "OLLAMA_BASE_URL")
}

func normalizeLocalServiceURL(value, variable string) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%s debe ser una URL HTTP(S) local sin ruta", variable)
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if !loopback {
		return "", fmt.Errorf("%s sólo puede apuntar a localhost o a una dirección IP de loopback", variable)
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}

func ValidateLocalModelName(value, variable string) (string, error) {
	model := strings.TrimSpace(value)
	if model == "" {
		return "", fmt.Errorf("%s no puede estar vacío", variable)
	}
	if strings.Contains(strings.ToLower(model), "cloud") {
		return "", fmt.Errorf("%s no puede seleccionar un modelo cloud; sólo se admiten modelos locales", variable)
	}
	return model, nil
}

func SlugifyVaultPath(vaultPath string) string {
	abs, _ := filepath.Abs(vaultPath)
	name := strings.ToLower(filepath.Base(abs))
	var builder strings.Builder
	lastDash := false
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			builder.WriteRune(r)
			lastDash = false
		} else if builder.Len() > 0 && !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		slug = "vault"
	}
	normalized := filepath.ToSlash(abs)
	digest := sha256.Sum256([]byte(normalized))
	return slug + "-" + hex.EncodeToString(digest[:])[:10]
}

func DefaultDatabasePath(baseDir, vaultPath string) string {
	return filepath.Join(baseDir, "data", "vaults", SlugifyVaultPath(vaultPath), "rag_index.sqlite3")
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("no se pudo leer .env: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			if value[0] == '"' {
				if decoded, decodeErr := strconv.Unquote(value); decodeErr == nil {
					value = decoded
				}
			} else {
				value = value[1 : len(value)-1]
			}
		}
		_ = os.Setenv(key, value)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("no se pudo interpretar .env: %w", err)
	}
	return nil
}

func resolvePath(baseDir, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("la ruta está vacía")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(baseDir, value)
	}
	return filepath.Abs(filepath.Clean(value))
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(env(name, strconv.Itoa(fallback))))
	if err != nil {
		return fallback
	}
	return value
}

func envFloat(name string, fallback float64) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(env(name, strconv.FormatFloat(fallback, 'f', -1, 64))), 64)
	if err != nil {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "sí", "si":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envList(name, fallback string) []string {
	values := strings.Split(env(name, fallback), ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if item := strings.TrimSpace(value); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func validateEnvironmentTypes() error {
	for _, name := range []string{
		"RAG_PARENT_CHILD_CHUNKING", "RAG_HYBRID_SEARCH", "RAG_MMR",
		"RAG_GRAPH_SEARCH", "RAG_RERANK", "RAG_QUERY_ROUTING", "RAG_QUERY_REWRITE",
		"RAG_READ_LARGE", "RAG_HUB_BOOST", "RAG_RECENCY_BOOST", "RAG_WATCH",
		"RAG_STREAMING", "RAG_AGENT_MODE", "RAG_VERIFIER_ABSTAIN_ON_INSUFFICIENT", "RAG_ANN",
	} {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on", "sí", "si", "0", "false", "no", "off":
		default:
			return fmt.Errorf("%s debe ser true/false, yes/no, on/off o 1/0", name)
		}
	}
	for _, name := range []string{
		"APP_PORT",
		"APP_READ_HEADER_TIMEOUT_SECONDS", "APP_IDLE_TIMEOUT_SECONDS", "APP_SHUTDOWN_TIMEOUT_SECONDS",
		"RAG_PARENT_CHUNK_SIZE", "RAG_CHILD_CHUNK_SIZE", "RAG_CHILD_CHUNK_OVERLAP",
		"RAG_CHUNK_SIZE", "RAG_CHUNK_OVERLAP", "RAG_EMBEDDING_WORKERS", "RAG_TOP_K",
		"RAG_ANN_M", "RAG_ANN_EF_CONSTRUCTION", "RAG_ANN_EF_SEARCH",
		"RAG_MAX_OUTPUT_TOKENS", "RAG_HYBRID_RRF_K", "RAG_HYBRID_CANDIDATE_MULTIPLIER",
		"RAG_MAX_CHUNKS_PER_DOCUMENT", "RAG_GRAPH_MAX_HOPS", "RAG_GRAPH_SEED_DOCUMENTS",
		"RAG_GRAPH_MAX_CANDIDATES", "RAG_RERANK_CANDIDATES", "RAG_QUERY_ROUTER_STRUCTURAL_THRESHOLD",
		"RAG_QUERY_REWRITE_CONTEXT_TOKENS", "RAG_QUERY_REWRITE_MAX_OUTPUT_TOKENS", "RAG_QUERY_REWRITE_MAX_MEMORY_CHARS",
		"RAG_AGENT_MAX_ITERATIONS", "RAG_AGENT_TOKEN_BUDGET", "RAG_AGENT_MAX_SUBQUESTIONS",
		"RAG_AGENT_CONTEXT_TOKENS", "RAG_AGENT_MAX_OBSERVATION_CHARS",
		"RAG_VERIFIER_CONTEXT_TOKENS", "RAG_WRITER_CONTEXT_TOKENS", "RAG_VERIFIER_MAX_OUTPUT_TOKENS",
		"RAG_MEMORY_SUMMARY_INTERVAL", "RAG_MEMORY_RECENT_TURNS", "RAG_MEMORY_MAX_CONTEXT_CHARS",
		"RAG_MEMORY_SUMMARY_MAX_INPUT_CHARS", "RAG_MEMORY_SUMMARY_MAX_TOKENS",
		"RAG_PROJECT_MEMORY_MAX_CONTEXT_CHARS", "RAG_PROJECT_MEMORY_SUMMARY_INTERVAL",
		"RAG_PROJECT_MEMORY_SUMMARY_MAX_INPUT_CHARS", "RAG_PROJECT_MEMORY_SUMMARY_MAX_TOKENS",
		"RAG_WATCH_POLL_INTERVAL_MS", "RAG_WATCH_DEBOUNCE_MS", "RAG_HISTORY_PAGE_SIZE", "RAG_HISTORY_MAX_PAGE_SIZE",
	} {
		if value, ok := os.LookupEnv(name); ok {
			if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
				return fmt.Errorf("%s debe ser un número entero", name)
			}
		}
	}
	for _, name := range []string{
		"OLLAMA_TIMEOUT_SECONDS", "RAG_MIN_SIMILARITY", "RAG_MIN_RELATIVE_SCORE",
		"RAG_HYBRID_SEMANTIC_WEIGHT", "RAG_HYBRID_LEXICAL_WEIGHT", "RAG_MMR_LAMBDA",
		"RAG_GRAPH_WEIGHT", "RAG_GRAPH_DECAY", "RAG_GRAPH_BACKLINK_WEIGHT", "RAG_RERANK_WEIGHT",
		"RAG_HUB_BOOST_WEIGHT", "RAG_RECENCY_BOOST_WEIGHT",
		"RAG_CROSS_ENCODER_TIMEOUT_SECONDS",
	} {
		if value, ok := os.LookupEnv(name); ok {
			if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
				return fmt.Errorf("%s debe ser un número", name)
			}
		}
	}
	return nil
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func clampInt(value, low, high int) int           { return min(high, max(low, value)) }
func clampFloat(value, low, high float64) float64 { return min(high, max(low, value)) }
func maxFloat(a, b float64) float64               { return max(a, b) }
