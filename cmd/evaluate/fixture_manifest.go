package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"obsigon/internal/config"
	"obsigon/internal/rag"
)

const fixtureFormatVersion = 1

// fixtureMetadata hace que un fixture sea una referencia auditable, no sólo
// una bolsa de respuestas. Los hashes evitan reproducir por accidente datos
// grabados con otro corpus, modelos o configuración de recuperación.
type fixtureMetadata struct {
	DatasetSHA256        string               `json:"dataset_sha256"`
	VaultSHA256          string               `json:"vault_sha256"`
	ChatModel            string               `json:"chat_model"`
	ChatModelSHA256      string               `json:"chat_model_sha256"`
	EmbeddingModel       string               `json:"embedding_model"`
	EmbeddingModelSHA256 string               `json:"embedding_model_sha256"`
	EmbeddingDimension   int                  `json:"embedding_dimension"`
	ChunkerVersion       string               `json:"chunker_version"`
	PrefixScheme         string               `json:"embedding_prefix_scheme"`
	ConfigurationSHA256  string               `json:"configuration_sha256"`
	Configuration        fixtureConfiguration `json:"configuration"`
}

// fixtureConfiguration enumera las opciones que pueden cambiar el corpus
// indexado, los candidatos recuperados o los prompts de la evaluación.
type fixtureConfiguration struct {
	ChunkSize                      int      `json:"chunk_size"`
	ChunkOverlap                   int      `json:"chunk_overlap"`
	ParentChildEnabled             bool     `json:"parent_child_enabled"`
	ParentChunkSize                int      `json:"parent_chunk_size"`
	ChildChunkSize                 int      `json:"child_chunk_size"`
	ChildChunkOverlap              int      `json:"child_chunk_overlap"`
	ANNM                           int      `json:"ann_m"`
	ANNEFConstruction              int      `json:"ann_ef_construction"`
	ANNEFSearch                    int      `json:"ann_ef_search"`
	TopK                           int      `json:"top_k"`
	MinSimilarity                  float64  `json:"min_similarity"`
	MinRelativeScore               float64  `json:"min_relative_score"`
	HybridSearchEnabled            bool     `json:"hybrid_search_enabled"`
	HybridRRFK                     int      `json:"hybrid_rrf_k"`
	HybridSemanticWeight           float64  `json:"hybrid_semantic_weight"`
	HybridLexicalWeight            float64  `json:"hybrid_lexical_weight"`
	HybridCandidateMultiplier      int      `json:"hybrid_candidate_multiplier"`
	MMREnabled                     bool     `json:"mmr_enabled"`
	MMRLambda                      float64  `json:"mmr_lambda"`
	MaxChunksPerDocument           int      `json:"max_chunks_per_document"`
	GraphSearchEnabled             bool     `json:"graph_search_enabled"`
	GraphWeight                    float64  `json:"graph_weight"`
	GraphMaxHops                   int      `json:"graph_max_hops"`
	GraphDecay                     float64  `json:"graph_decay"`
	GraphBacklinkWeight            float64  `json:"graph_backlink_weight"`
	GraphSeedDocuments             int      `json:"graph_seed_documents"`
	GraphMaxCandidates             int      `json:"graph_max_candidates"`
	RerankEnabled                  bool     `json:"rerank_enabled"`
	RerankBackend                  string   `json:"rerank_backend"`
	RerankCandidates               int      `json:"rerank_candidates"`
	RerankWeight                   float64  `json:"rerank_weight"`
	CrossEncoderModel              string   `json:"cross_encoder_model"`
	QueryRoutingEnabled            bool     `json:"query_routing_enabled"`
	QueryRouterRelationalPatterns  []string `json:"query_router_relational_patterns"`
	QueryRouterStructuralTerms     []string `json:"query_router_structural_terms"`
	QueryRouterStructuralThreshold int      `json:"query_router_structural_threshold"`
	QueryRewriteEnabled            bool     `json:"query_rewrite_enabled"`
	QueryRewriteContextTokens      int      `json:"query_rewrite_context_tokens"`
	QueryRewriteMaxOutputTokens    int      `json:"query_rewrite_max_output_tokens"`
	QueryRewriteMaxMemoryChars     int      `json:"query_rewrite_max_memory_chars"`
	ReadLargeEnabled               bool     `json:"read_large_enabled"`
	HubBoostEnabled                bool     `json:"hub_boost_enabled"`
	HubBoostWeight                 float64  `json:"hub_boost_weight"`
	RecencyBoostEnabled            bool     `json:"recency_boost_enabled"`
	RecencyBoostWeight             float64  `json:"recency_boost_weight"`
	AgentMode                      bool     `json:"agent_mode"`
	AgentMaxIterations             int      `json:"agent_max_iterations"`
	AgentTokenBudget               int      `json:"agent_token_budget"`
	AgentMaxSubquestions           int      `json:"agent_max_subquestions"`
	AgentContextTokens             int      `json:"agent_context_tokens"`
	AgentMaxObservationChars       int      `json:"agent_max_observation_chars"`
	VerifierContextTokens          int      `json:"verifier_context_tokens"`
	WriterContextTokens            int      `json:"writer_context_tokens"`
	VerifierMaxOutputTokens        int      `json:"verifier_max_output_tokens"`
	VerifierAbstainOnInsufficient  bool     `json:"verifier_abstain_on_insufficient"`
	MaxOutputTokens                int      `json:"max_output_tokens"`
}

func buildFixtureConfiguration(settings config.Settings) fixtureConfiguration {
	return fixtureConfiguration{
		ChunkSize: settings.ChunkSize, ChunkOverlap: settings.ChunkOverlap,
		ParentChildEnabled: settings.ParentChildEnabled, ParentChunkSize: settings.ParentChunkSize,
		ChildChunkSize: settings.ChildChunkSize, ChildChunkOverlap: settings.ChildChunkOverlap,
		ANNM: settings.ANNM, ANNEFConstruction: settings.ANNEFConstruction, ANNEFSearch: settings.ANNEFSearch,
		TopK: settings.TopK, MinSimilarity: settings.MinSimilarity, MinRelativeScore: settings.MinRelativeScore,
		HybridSearchEnabled: settings.HybridSearchEnabled, HybridRRFK: settings.HybridRRFK,
		HybridSemanticWeight: settings.HybridSemanticWeight, HybridLexicalWeight: settings.HybridLexicalWeight,
		HybridCandidateMultiplier: settings.HybridCandidateMultiplier,
		MMREnabled:                settings.MMREnabled, MMRLambda: settings.MMRLambda, MaxChunksPerDocument: settings.MaxChunksPerDocument,
		GraphSearchEnabled: settings.GraphSearchEnabled, GraphWeight: settings.GraphWeight,
		GraphMaxHops: settings.GraphMaxHops, GraphDecay: settings.GraphDecay,
		GraphBacklinkWeight: settings.GraphBacklinkWeight, GraphSeedDocuments: settings.GraphSeedDocuments,
		GraphMaxCandidates: settings.GraphMaxCandidates,
		RerankEnabled:      settings.RerankEnabled, RerankBackend: settings.RerankBackend,
		RerankCandidates: settings.RerankCandidates, RerankWeight: settings.RerankWeight,
		CrossEncoderModel:              settings.CrossEncoderModel,
		QueryRoutingEnabled:            settings.QueryRoutingEnabled,
		QueryRouterRelationalPatterns:  append([]string(nil), settings.QueryRouterRelationalPatterns...),
		QueryRouterStructuralTerms:     append([]string(nil), settings.QueryRouterStructuralTerms...),
		QueryRouterStructuralThreshold: settings.QueryRouterStructuralThreshold,
		QueryRewriteEnabled:            settings.QueryRewriteEnabled, QueryRewriteContextTokens: settings.QueryRewriteContextTokens,
		QueryRewriteMaxOutputTokens: settings.QueryRewriteMaxOutputTokens,
		QueryRewriteMaxMemoryChars:  settings.QueryRewriteMaxMemoryChars,
		ReadLargeEnabled:            settings.ReadLargeEnabled, HubBoostEnabled: settings.HubBoostEnabled,
		HubBoostWeight: settings.HubBoostWeight, RecencyBoostEnabled: settings.RecencyBoostEnabled,
		RecencyBoostWeight: settings.RecencyBoostWeight,
		AgentMode:          settings.AgentMode, AgentMaxIterations: settings.AgentMaxIterations,
		AgentTokenBudget: settings.AgentTokenBudget, AgentMaxSubquestions: settings.AgentMaxSubquestions,
		AgentContextTokens: settings.AgentContextTokens, AgentMaxObservationChars: settings.AgentMaxObservationChars,
		VerifierContextTokens: settings.VerifierContextTokens, WriterContextTokens: settings.WriterContextTokens,
		VerifierMaxOutputTokens:       settings.VerifierMaxOutputTokens,
		VerifierAbstainOnInsufficient: settings.VerifierAbstainOnInsufficient,
		MaxOutputTokens:               settings.MaxOutputTokens,
	}
}

func finalizeFixtureData(data *fixtureData, datasetPath string, datasetRaw []byte, vaultPath string, settings config.Settings) error {
	if data.Dimension <= 0 {
		return fmt.Errorf("no se puede guardar el fixture: dimensión de embeddings no válida: %d", data.Dimension)
	}
	metadata, err := expectedFixtureMetadata(datasetRaw, vaultPath, settings, data.Dimension)
	if err != nil {
		return err
	}
	metadata.ChatModelSHA256, err = recordedModelSHA256(data, settings.ChatModel)
	if err != nil {
		return err
	}
	metadata.EmbeddingModelSHA256, err = recordedModelSHA256(data, settings.EmbeddingModel)
	if err != nil {
		return err
	}
	data.FormatVersion = fixtureFormatVersion
	data.Dataset = filepath.Base(datasetPath)
	data.Metadata = metadata
	return validateFixtureContents(data)
}

func validateFixtureData(data *fixtureData, datasetPath string, datasetRaw []byte, vaultPath string, settings config.Settings) error {
	if data.FormatVersion != fixtureFormatVersion {
		return fmt.Errorf("fixture incompatible: versión %d, esperada %d; regenera con `-record`", data.FormatVersion, fixtureFormatVersion)
	}
	if data.Dataset != filepath.Base(datasetPath) {
		return fmt.Errorf("fixture incompatible: dataset %q, esperado %q; regenera con `-record`", data.Dataset, filepath.Base(datasetPath))
	}
	if data.Dimension <= 0 || data.Metadata.EmbeddingDimension != data.Dimension {
		return fmt.Errorf("fixture incompatible: dimensión de embeddings inconsistente; regenera con `-record`")
	}
	expected, err := expectedFixtureMetadata(datasetRaw, vaultPath, settings, data.Dimension)
	if err != nil {
		return err
	}
	checks := []struct {
		name, got, want string
	}{
		{"dataset", data.Metadata.DatasetSHA256, expected.DatasetSHA256},
		{"vault", data.Metadata.VaultSHA256, expected.VaultSHA256},
		{"modelo de chat", data.Metadata.ChatModel, expected.ChatModel},
		{"modelo de embeddings", data.Metadata.EmbeddingModel, expected.EmbeddingModel},
		{"chunker", data.Metadata.ChunkerVersion, expected.ChunkerVersion},
		{"prefijos de embeddings", data.Metadata.PrefixScheme, expected.PrefixScheme},
		{"configuración", data.Metadata.ConfigurationSHA256, expected.ConfigurationSHA256},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("fixture incompatible: cambió %s (fixture=%q, actual=%q); regenera con `-record`", check.name, check.got, check.want)
		}
	}
	storedConfigurationSHA, err := jsonSHA256(data.Metadata.Configuration)
	if err != nil || storedConfigurationSHA != data.Metadata.ConfigurationSHA256 {
		return fmt.Errorf("fixture corrupto: la configuración no coincide con su hash; regenera con `-record`")
	}
	for _, model := range []struct{ name, digest string }{
		{data.Metadata.ChatModel, data.Metadata.ChatModelSHA256},
		{data.Metadata.EmbeddingModel, data.Metadata.EmbeddingModelSHA256},
	} {
		digest, digestErr := recordedModelSHA256(data, model.name)
		if digestErr != nil || digest != model.digest {
			return fmt.Errorf("fixture corrupto: identidad incompleta o alterada para el modelo %q; regenera con `-record`", model.name)
		}
	}
	return validateFixtureContents(data)
}

func expectedFixtureMetadata(datasetRaw []byte, vaultPath string, settings config.Settings, dimension int) (fixtureMetadata, error) {
	datasetSHA, err := canonicalJSONSHA256(datasetRaw)
	if err != nil {
		return fixtureMetadata{}, fmt.Errorf("calcular firma del dataset: %w", err)
	}
	vaultSHA, err := vaultSHA256(vaultPath)
	if err != nil {
		return fixtureMetadata{}, fmt.Errorf("calcular firma del vault: %w", err)
	}
	scheme, err := rag.ResolveScheme(settings.EmbeddingModel, settings.EmbeddingPrefixScheme)
	if err != nil {
		return fixtureMetadata{}, err
	}
	configuration := buildFixtureConfiguration(settings)
	configurationSHA, err := jsonSHA256(configuration)
	if err != nil {
		return fixtureMetadata{}, err
	}
	return fixtureMetadata{
		DatasetSHA256: datasetSHA, VaultSHA256: vaultSHA,
		ChatModel: settings.ChatModel, EmbeddingModel: settings.EmbeddingModel,
		EmbeddingDimension: dimension, ChunkerVersion: rag.ChunkerVersion,
		PrefixScheme: scheme.ID, ConfigurationSHA256: configurationSHA,
		Configuration: configuration,
	}, nil
}

func validateFixtureContents(data *fixtureData) error {
	if len(data.Embed) == 0 {
		return fmt.Errorf("fixture incompleto: no contiene embeddings; regenera con `-record`")
	}
	for key, batches := range data.Embed {
		if len(batches) == 0 {
			return fmt.Errorf("fixture incompleto: lote de embeddings %s vacío; regenera con `-record`", key)
		}
		for _, vector := range batches {
			if len(vector) != data.Dimension {
				return fmt.Errorf("fixture incompatible: vector de dimensión %d, esperada %d; regenera con `-record`", len(vector), data.Dimension)
			}
			for _, value := range vector {
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
					return fmt.Errorf("fixture corrupto: embedding no finito; regenera con `-record`")
				}
			}
		}
	}
	return nil
}

func recordedModelSHA256(data *fixtureData, model string) (string, error) {
	raw, ok := data.Model["modelshow\x00"+model]
	if !ok || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
		return "", fmt.Errorf("fixture incompleto: faltan metadatos del modelo %q; regenera con `-record`", model)
	}
	return canonicalJSONSHA256(raw)
}

func canonicalJSONSHA256(raw []byte) (string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return jsonSHA256(value)
}

func jsonSHA256(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return bytesSHA256(raw), nil
}

func vaultSHA256(vaultPath string) (string, error) {
	files, err := rag.DiscoverMarkdownFiles(vaultPath)
	if err != nil {
		return "", err
	}
	type vaultFile struct{ relative, path string }
	ordered := make([]vaultFile, 0, len(files))
	for _, path := range files {
		relative, relErr := filepath.Rel(vaultPath, path)
		if relErr != nil {
			return "", relErr
		}
		ordered = append(ordered, vaultFile{relative: filepath.ToSlash(relative), path: path})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].relative < ordered[j].relative })
	hash := sha256.New()
	for _, file := range ordered {
		raw, readErr := os.ReadFile(file.path)
		if readErr != nil {
			return "", readErr
		}
		normalized := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		hash.Write([]byte(file.relative))
		hash.Write([]byte{0})
		hash.Write(normalized)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func bytesSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func backendReport(report evaluationReport) map[string]any {
	return map[string]any{
		"cases": report.Cases, "passes": report.Passes,
		"mean_reciprocal_rank": report.MeanReciprocalRank,
		"metrics":              report.Metrics, "results": report.Results,
	}
}

func requireSemanticBackend(report evaluationReport, expected string) error {
	for _, result := range report.Results {
		if result.SemanticBackend != expected {
			return fmt.Errorf("caso %s: se esperaba backend semántico %s y se usó %s", result.ID, expected, result.SemanticBackend)
		}
	}
	return nil
}

func normalizedModelName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
