// Command evaluate mide la recuperacion sobre los datasets JSON de evaluations/.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

type dataset struct {
	Cases []evaluationCase `json:"cases"`
}

type evaluationCase struct {
	ID, Question         string
	ExpectedPaths        []string   `json:"expected_paths"`
	ForbiddenPaths       []string   `json:"forbidden_paths"`
	ExpectedTerms        [][]string `json:"expected_terms"`
	Memory               string     `json:"memory,omitempty"`
	ExpectedRewriteTerms []string   `json:"expected_rewrite_terms,omitempty"`
	ExpectAbstention     bool       `json:"expect_abstention"`
	Status, Vigencia     string
	Tags                 []string
	Frontmatter          map[string]any `json:"frontmatter,omitempty"`
	ExpandLinks          *bool          `json:"expand_links"`
}

type caseResult struct {
	ID                 string              `json:"id"`
	RetrievedPaths     []string            `json:"retrieved_paths"`
	ReciprocalRank     *float64            `json:"reciprocal_rank"`
	DocumentRecall     *float64            `json:"document_recall"`
	RecallAtK          *float64            `json:"recall_at_k"`
	PrecisionAtK       *float64            `json:"precision_at_k"`
	NDCGAtK            *float64            `json:"ndcg_at_k"`
	RetrievalPass      bool                `json:"retrieval_pass"`
	Answer             string              `json:"answer,omitempty"`
	AnswerPass         *bool               `json:"answer_pass,omitempty"`
	AnswerTermRecall   *float64            `json:"answer_term_recall,omitempty"`
	MatchedTermGroups  int                 `json:"matched_term_groups,omitempty"`
	ExpectedTermGroups int                 `json:"expected_term_groups,omitempty"`
	Citations          *rag.CitationReport `json:"citations,omitempty"`
	Pass               bool                `json:"pass"`
	Abstained          bool                `json:"abstained"`
	ExpectAbstention   bool                `json:"expect_abstention"`
	AutonomousQuestion string              `json:"autonomous_question,omitempty"`
	QueryRewriteStatus string              `json:"query_rewrite_status,omitempty"`
	QueryRewritePass   *bool               `json:"query_rewrite_pass,omitempty"`
	SemanticBackend    string              `json:"semantic_backend"`
}

type evaluationReport struct {
	Cases              int            `json:"cases"`
	Passes             int            `json:"passes"`
	MeanReciprocalRank *float64       `json:"mean_reciprocal_rank"`
	Metrics            map[string]any `json:"metrics"`
	Results            []caseResult   `json:"results"`
}

func main() {
	vault := flag.String("vault", "", "Vault evaluado")
	dataPath := flag.String("dataset", "", "JSON de evaluacion")
	compareStrategies := flag.Bool("compare-strategies", false, "Compara graph_off, graph_on_1hop y router_real")
	comparePhaseOne := flag.Bool("compare-phase1", false, "Compara la recuperación anterior con reformulación, lectura amplia y señales estructurales")
	fixturesPath := flag.String("fixtures", "", "JSON de respuestas de Ollama grabadas; sin -record se reproduce sin red")
	record := flag.Bool("record", false, "Graba las respuestas reales de Ollama en -fixtures en vez de reproducir")
	check := flag.Bool("check", false, "Compara las métricas con -thresholds y sale con código ≠ 0 si alguna cae por debajo")
	thresholdsPath := flag.String("thresholds", filepath.Join("evaluations", "thresholds.json"), "JSON de pisos mínimos por métrica para -check")
	flag.Parse()
	if *vault == "" || *dataPath == "" {
		fmt.Fprintln(os.Stderr, "faltan -vault y/o -dataset")
		os.Exit(2)
	}
	if *check && (*compareStrategies || *comparePhaseOne) {
		fmt.Fprintln(os.Stderr, "-check sólo aplica a la evaluación simple, no a los modos -compare-*")
		os.Exit(2)
	}
	resolvedFixtures := *fixturesPath
	if resolvedFixtures == "" && *record {
		resolvedFixtures = filepath.Join("evaluations", "fixtures", strings.TrimSuffix(filepath.Base(*dataPath), filepath.Ext(*dataPath))+".json")
	}
	if *check && resolvedFixtures == "" {
		fmt.Fprintln(os.Stderr, "-check requiere -fixtures; la puerta de calidad no puede usar Ollama en vivo")
		os.Exit(2)
	}
	replaying := resolvedFixtures != "" && !*record
	base, err := os.Getwd()
	fatal(err)
	settings, err := config.Load(base)
	fatal(err)
	settings.VaultPath = mustAbs(*vault)
	raw, err := os.ReadFile(*dataPath)
	fatal(err)
	var data dataset
	fatal(json.Unmarshal(raw, &data))
	temporary, err := os.MkdirTemp("", "obsigon-eval-")
	fatal(err)
	defer os.RemoveAll(temporary)
	settings.DatabasePath = filepath.Join(temporary, "evaluation.sqlite3")
	settings.DatabasePathExplicit = true
	store, err := db.Open(settings.DatabasePath)
	fatal(err)
	defer store.Close()
	client := ollama.New(settings.OllamaBaseURL, settings.ChatModel, settings.EmbeddingModel, settings.OllamaKeepAlive, time.Duration(settings.OllamaTimeoutSeconds*float64(time.Second)), settings.EmbeddingWorkers)
	var eval evalClient = client
	var recorder *recordingClient
	switch {
	case replaying:
		fixture, loadErr := loadFixtureData(resolvedFixtures)
		fatal(loadErr)
		fatal(validateFixtureData(fixture, *dataPath, raw, settings.VaultPath, settings))
		eval = &replayClient{data: fixture}
	case *record:
		recorder = &recordingClient{inner: client, data: newFixtureData(filepath.Base(*dataPath))}
		eval = recorder
	}
	indexer := rag.Indexer{Settings: settings, Store: store, Embedder: eval}
	indexResult, err := indexer.Index(context.Background())
	fatal(err)
	newVectors := func(enabled bool, name string) *rag.VectorStore {
		return rag.NewVectorStore(store, settings.EmbeddingWorkers, rag.ANNConfig{
			Enabled: enabled, Path: filepath.Join(temporary, name+".hnsw"),
			M: settings.ANNM, EFConstruction: settings.ANNEFConstruction, EFSearch: settings.ANNEFSearch,
		})
	}
	vectors := newVectors(settings.ANNEnabled, "configured")

	var payload map[string]any
	checkReports := map[string]evaluationReport{}
	if *comparePhaseOne {
		baseline := settings
		baseline.QueryRewriteEnabled = false
		baseline.ReadLargeEnabled = false
		baseline.HubBoostEnabled = false
		baseline.RecencyBoostEnabled = false
		phaseOne := settings
		phaseOne.QueryRewriteEnabled = true
		phaseOne.ReadLargeEnabled = true
		phaseOne.HubBoostEnabled = true
		phaseOne.RecencyBoostEnabled = true
		baselineReport, err := evaluate(context.Background(), data.Cases, baseline, store, vectors, eval, eval)
		fatal(err)
		phaseOneReport, err := evaluate(context.Background(), data.Cases, phaseOne, store, vectors, eval, eval)
		fatal(err)
		payload = map[string]any{
			"index": indexResult,
			"strategies": map[string]any{
				"baseline": map[string]any{"metrics": baselineReport.Metrics, "results": baselineReport.Results},
				"phase_1":  map[string]any{"metrics": phaseOneReport.Metrics, "results": phaseOneReport.Results},
			},
		}
	} else if *compareStrategies {
		off := settings
		off.GraphSearchEnabled = false
		on := settings
		on.GraphSearchEnabled = true
		on.GraphMaxHops = 1
		router := settings
		router.GraphSearchEnabled = false
		router.QueryRoutingEnabled = true
		offReport, err := evaluate(context.Background(), data.Cases, off, store, vectors, eval, eval)
		fatal(err)
		onReport, err := evaluate(context.Background(), data.Cases, on, store, vectors, eval, eval)
		fatal(err)
		routerReport, err := evaluate(context.Background(), data.Cases, router, store, vectors, eval, eval)
		fatal(err)
		payload = map[string]any{
			"index": indexResult,
			"strategies": map[string]any{
				"graph_off":     map[string]any{"metrics": offReport.Metrics},
				"graph_on_1hop": map[string]any{"metrics": onReport.Metrics},
				"router_real":   map[string]any{"metrics": routerReport.Metrics},
			},
		}
	} else if *check || *record {
		exactSettings := settings
		exactSettings.ANNEnabled = false
		exactReport, err := evaluate(context.Background(), data.Cases, exactSettings, store, newVectors(false, "quality-exact"), eval, eval)
		fatal(err)
		fatal(requireSemanticBackend(exactReport, "exact"))

		annSettings := settings
		annSettings.ANNEnabled = true
		annReport, err := evaluate(context.Background(), data.Cases, annSettings, store, newVectors(true, "quality-ann"), eval, eval)
		fatal(err)
		fatal(requireSemanticBackend(annReport, "ann"))

		payload = map[string]any{
			"index": indexResult,
			"backends": map[string]any{
				"exact": backendReport(exactReport),
				"ann":   backendReport(annReport),
			},
		}
		checkReports["exact"] = exactReport
		checkReports["ann"] = annReport
	} else {
		report, err := evaluate(context.Background(), data.Cases, settings, store, vectors, eval, eval)
		fatal(err)
		payload = map[string]any{
			"index":                indexResult,
			"cases":                report.Cases,
			"passes":               report.Passes,
			"mean_reciprocal_rank": report.MeanReciprocalRank,
			"metrics":              report.Metrics,
			"results":              report.Results,
		}
	}
	if recorder != nil {
		_, err = recorder.ModelShow(context.Background(), settings.ChatModel)
		fatal(err)
		_, err = recorder.ModelShow(context.Background(), settings.EmbeddingModel)
		fatal(err)
		fatal(finalizeFixtureData(recorder.data, *dataPath, raw, settings.VaultPath, settings))
		fatal(recorder.data.save(resolvedFixtures))
		fmt.Fprintf(os.Stderr, "fixtures grabados en %s (embed=%d chat=%d)\n", resolvedFixtures, len(recorder.data.Embed), len(recorder.data.Chat))
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	fatal(encoder.Encode(payload))

	if *check {
		allPass := true
		for _, backend := range []string{"exact", "ann"} {
			report, exists := checkReports[backend]
			if !exists {
				fatal(fmt.Errorf("la puerta de calidad no ejecutó el backend %s", backend))
			}
			summary, ok, checkErr := checkThresholds(report.Metrics, *thresholdsPath)
			fatal(checkErr)
			fmt.Fprintf(os.Stderr, "\nPuerta de calidad %s (%s):\n%s", backend, *thresholdsPath, summary)
			allPass = allPass && ok
		}
		if !allPass {
			fmt.Fprintln(os.Stderr, "RESULTADO: uno o más backends tienen métricas por debajo del piso")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "RESULTADO: exact y ann cumplen todos los pisos")
	}
}

func evaluate(ctx context.Context, cases []evaluationCase, settings config.Settings, store *db.Store, vectors *rag.VectorStore, embedder rag.Embedder, generator rag.AgentGenerator) (evaluationReport, error) {
	reranker := rag.NewNoopReranker()
	switch settings.RerankBackend {
	case "llm":
		reranker = rag.NewLLMReranker(generator, settings.RerankEnabled, settings.RerankCandidates, settings.RerankWeight)
	case "cross_encoder":
		reranker = rag.NewCrossEncoderReranker(settings.CrossEncoderURL, settings.CrossEncoderModel, settings.RerankEnabled, settings.RerankCandidates, settings.RerankWeight, time.Duration(settings.CrossEncoderTimeoutSeconds*float64(time.Second)))
	}
	retriever := rag.Retriever{Settings: settings, Store: store, Vectors: vectors, Embedder: embedder, Reranker: reranker}
	modelContext := rag.ResolveModelContext(ctx, settings.ChatModel, generator)
	results := make([]caseResult, 0, len(cases))
	for _, item := range cases {
		expandLinks := true
		if item.ExpandLinks != nil {
			expandLinks = *item.ExpandLinks
		}
		autonomousQuestion := strings.TrimSpace(item.Question)
		rewriteStatus := "not_needed"
		if !settings.AgentMode {
			rewrite := rag.RewriteStandaloneQuestion(ctx, generator, item.Question, item.Memory, rag.QueryRewriteOptions{
				Enabled: settings.QueryRewriteEnabled, ContextTokens: settings.QueryRewriteContextTokens,
				MaxOutputTokens: settings.QueryRewriteMaxOutputTokens, MaxMemoryChars: settings.QueryRewriteMaxMemoryChars,
			})
			autonomousQuestion = rewrite.Question
			rewriteStatus = fmt.Sprint(rewrite.Metrics["status"])
		} else if strings.TrimSpace(item.Memory) != "" {
			rewriteStatus = "covered_by_agentic_decomposition"
		}
		requestQuestion := autonomousQuestion
		if settings.AgentMode {
			requestQuestion = strings.TrimSpace(item.Question)
		}
		request := rag.RetrievalRequest{Question: requestQuestion, SemanticQuestion: requestQuestion, TopK: settings.TopK, MinSimilarity: settings.MinSimilarity, Status: item.Status, Vigencia: item.Vigencia, Tags: item.Tags, Frontmatter: item.Frontmatter, ExpandLinks: expandLinks}
		execution := rag.AgenticRetrievalResult{}
		var err error
		if settings.AgentMode {
			execution, err = rag.RunAgenticRetrieval(ctx, generator, &retriever, request, rag.AgenticRetrievalOptions{
				MaxIterations: settings.AgentMaxIterations, TokenBudget: settings.AgentTokenBudget,
				MaxSubquestions: settings.AgentMaxSubquestions, ContextTokens: settings.AgentContextTokens,
				MaxObservationChars: settings.AgentMaxObservationChars,
				ConversationMemory:  item.Memory,
			})
		} else {
			execution.Retrieval, err = retriever.Retrieve(ctx, request)
		}
		if err != nil {
			return evaluationReport{}, err
		}
		result := execution.Retrieval
		paths := make([]string, 0, len(result.Hits))
		for _, hit := range result.Hits {
			paths = append(paths, normalize(fmt.Sprint(hit["path"])))
		}
		scored := scoreCase(item, paths, result.Abstained)
		scored.AutonomousQuestion = autonomousQuestion
		scored.QueryRewriteStatus = rewriteStatus
		scored.SemanticBackend = result.Trace.SemanticBackend
		if len(item.ExpectedRewriteTerms) > 0 {
			passed := containsAllNormalized(autonomousQuestion, item.ExpectedRewriteTerms)
			scored.QueryRewritePass = &passed
			scored.Pass = scored.Pass && passed
		}
		if !item.ExpectAbstention {
			pipeline, pipelineErr := rag.RunAgentPipeline(ctx, generator, item.Question, result.Hits, rag.AgentOptions{
				ResearchBrief:           execution.ResearchBrief,
				ConversationMemory:      item.Memory,
				VerificationEnabled:     true,
				VerifierContextTokens:   settings.VerifierContextTokens,
				WriterContextTokens:     settings.WriterContextTokens,
				VerifierMaxOutputTokens: settings.VerifierMaxOutputTokens,
				WriterMaxOutputTokens:   settings.MaxOutputTokens,
				AbstainOnInsufficient:   settings.VerifierAbstainOnInsufficient,
				ModelContext:            modelContext,
			})
			if pipelineErr != nil {
				return evaluationReport{}, fmt.Errorf("caso %s: generar respuesta: %w", item.ID, pipelineErr)
			}
			scoreAnswer(&scored, item, pipeline.Answer, len(result.Hits))
		}
		results = append(results, scored)
	}
	metrics := summarize(results)
	passes := 0
	for _, result := range results {
		if result.Pass {
			passes++
		}
	}
	mrr, _ := metrics["mean_reciprocal_rank"].(*float64)
	return evaluationReport{Cases: len(cases), Passes: passes, MeanReciprocalRank: mrr, Metrics: metrics, Results: results}, nil
}

func scoreCase(item evaluationCase, paths []string, abstained bool) caseResult {
	result := caseResult{ID: item.ID, RetrievedPaths: paths, Abstained: abstained, ExpectAbstention: item.ExpectAbstention}
	if item.ExpectAbstention {
		result.RetrievalPass = len(paths) == 0
		result.Pass = result.RetrievalPass
		return result
	}
	expected := normalizedSet(item.ExpectedPaths)
	forbidden := normalizedSet(item.ForbiddenPaths)
	matched := map[string]bool{}
	bestRank := 0
	precisionHits := 0
	leaked := false
	for index, path := range paths {
		if expected[path] {
			matched[path] = true
			precisionHits++
			if bestRank == 0 {
				bestRank = index + 1
			}
		}
		if forbidden[path] {
			leaked = true
		}
	}
	recall := 0.0
	if len(expected) > 0 {
		recall = float64(len(matched)) / float64(len(expected))
	}
	precision := 0.0
	if len(paths) > 0 {
		precision = float64(precisionHits) / float64(len(paths))
	}
	reciprocal := 0.0
	if bestRank > 0 {
		reciprocal = 1 / float64(bestRank)
	}
	ndcg := ndcgAtK(paths, expected)
	result.DocumentRecall = &recall
	result.RecallAtK = &recall
	result.PrecisionAtK = &precision
	result.ReciprocalRank = &reciprocal
	result.NDCGAtK = &ndcg
	result.RetrievalPass = recall == 1 && !leaked
	result.Pass = result.RetrievalPass
	return result
}

func scoreAnswer(result *caseResult, item evaluationCase, answer string, sourceCount int) {
	cleaned, citations := rag.ValidateAnswer(answer, sourceCount)
	result.Answer = cleaned
	result.Citations = &citations
	matched, total := matchedExpectedTermGroups(cleaned, item.ExpectedTerms)
	result.MatchedTermGroups = matched
	result.ExpectedTermGroups = total
	termsPass := true
	if total > 0 {
		recall := float64(matched) / float64(total)
		result.AnswerTermRecall = &recall
		termsPass = matched == total
	}
	citationsPass := sourceCount > 0 && !citations.HasInvalid && !citations.UncitedAnswer
	answerPass := termsPass && citationsPass
	result.AnswerPass = &answerPass
	rewritePass := result.QueryRewritePass == nil || *result.QueryRewritePass
	result.Pass = result.RetrievalPass && answerPass && rewritePass
}

func containsAllNormalized(value string, terms []string) bool {
	normalized := normalizeText(value)
	for _, term := range terms {
		wanted := normalizeText(term)
		if wanted != "" && !strings.Contains(normalized, wanted) {
			return false
		}
	}
	return true
}

func matchedExpectedTermGroups(answer string, groups [][]string) (int, int) {
	normalizedAnswer := normalizeText(answer)
	matched, total := 0, 0
	for _, alternatives := range groups {
		nonEmpty := false
		groupMatched := false
		for _, term := range alternatives {
			normalizedTerm := normalizeText(term)
			if normalizedTerm == "" {
				continue
			}
			nonEmpty = true
			if strings.Contains(normalizedAnswer, normalizedTerm) {
				groupMatched = true
			}
		}
		if !nonEmpty {
			continue
		}
		total++
		if groupMatched {
			matched++
		}
	}
	return matched, total
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func summarize(results []caseResult) map[string]any {
	answerable, abstentionCases := 0, 0
	exactHits, answeredAnswerable := 0, 0
	abstentionHits, falseAbstentions := 0, 0
	recallTotal, precisionTotal, ndcgTotal, mrrTotal := 0.0, 0.0, 0.0, 0.0
	answerPasses, citationPasses, termCases, endToEndPasses := 0, 0, 0, 0
	rewriteCases, rewritePasses := 0, 0
	termRecallTotal := 0.0
	answered := 0
	for _, result := range results {
		if result.QueryRewritePass != nil {
			rewriteCases++
			if *result.QueryRewritePass {
				rewritePasses++
			}
		}
		if len(result.RetrievedPaths) > 0 {
			answered++
		}
		if result.ExpectAbstention {
			abstentionCases++
			if result.Pass {
				abstentionHits++
			}
			continue
		}
		answerable++
		if len(result.RetrievedPaths) > 0 {
			answeredAnswerable++
		} else {
			falseAbstentions++
		}
		if result.RetrievalPass {
			exactHits++
		}
		if result.AnswerPass != nil {
			if *result.AnswerPass {
				answerPasses++
			}
			if result.Citations != nil && !result.Citations.HasInvalid && !result.Citations.UncitedAnswer {
				citationPasses++
			}
		}
		if result.AnswerTermRecall != nil {
			termCases++
			termRecallTotal += *result.AnswerTermRecall
		}
		if result.Pass {
			endToEndPasses++
		}
		recallTotal += deref(result.DocumentRecall)
		precisionTotal += deref(result.PrecisionAtK)
		ndcgTotal += deref(result.NDCGAtK)
		mrrTotal += deref(result.ReciprocalRank)
	}
	metrics := map[string]any{
		"cases":                   len(results),
		"answerable_cases":        answerable,
		"abstention_cases":        abstentionCases,
		"coverage":                ratio(answered, len(results)),
		"abstention_recall":       ratio(abstentionHits, abstentionCases),
		"answer_pass_rate":        ratio(answerPasses, answerable),
		"citation_valid_rate":     ratio(citationPasses, answerable),
		"answer_term_recall":      ratioFloat(termRecallTotal, termCases),
		"end_to_end_pass_rate":    ratio(endToEndPasses+abstentionHits, len(results)),
		"query_rewrite_pass_rate": ratio(rewritePasses, rewriteCases),
	}
	if answerable > 0 {
		metrics["retrieval_exact_hit_rate"] = float64(exactHits) / float64(answerable)
		metrics["document_recall"] = recallTotal / float64(answerable)
		metrics["recall_at_k"] = recallTotal / float64(answerable)
		metrics["precision_at_k"] = precisionTotal / float64(answerable)
		metrics["ndcg_at_k"] = ndcgTotal / float64(answerable)
		mrr := mrrTotal / float64(answerable)
		metrics["mean_reciprocal_rank"] = &mrr
		metrics["accuracy_given_answered"] = ratio(exactHits, answeredAnswerable)
	} else {
		metrics["retrieval_exact_hit_rate"] = nil
		metrics["document_recall"] = nil
		metrics["recall_at_k"] = nil
		metrics["precision_at_k"] = nil
		metrics["ndcg_at_k"] = nil
		metrics["mean_reciprocal_rank"] = nil
		metrics["accuracy_given_answered"] = nil
	}
	precisionDenominator := abstentionHits + falseAbstentions
	metrics["abstention_precision"] = ratio(abstentionHits, precisionDenominator)
	if precisionDenominator > 0 && abstentionCases > 0 {
		precision := float64(abstentionHits) / float64(precisionDenominator)
		recall := float64(abstentionHits) / float64(abstentionCases)
		if precision+recall > 0 {
			metrics["abstention_f1"] = 2 * precision * recall / (precision + recall)
		} else {
			metrics["abstention_f1"] = nil
		}
	} else {
		metrics["abstention_f1"] = nil
	}
	return metrics
}

func ratioFloat(numerator float64, denominator int) any {
	if denominator == 0 {
		return nil
	}
	return numerator / float64(denominator)
}

func ndcgAtK(paths []string, expected map[string]bool) float64 {
	seen := map[string]bool{}
	deduplicated := make([]string, 0, len(paths))
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			deduplicated = append(deduplicated, path)
		}
	}
	dcg := 0.0
	for index, path := range deduplicated {
		if expected[path] {
			dcg += 1 / math.Log2(float64(index+2))
		}
	}
	idealHits := min(len(expected), len(deduplicated))
	idcg := 0.0
	for index := 0; index < idealHits; index++ {
		idcg += 1 / math.Log2(float64(index+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func normalizedSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if normalized := normalize(value); normalized != "" {
			result[normalized] = true
		}
	}
	return result
}

func ratio(numerator, denominator int) any {
	if denominator == 0 {
		return nil
	}
	return float64(numerator) / float64(denominator)
}

func deref(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
}

func mustAbs(value string) string {
	absolute, err := filepath.Abs(value)
	fatal(err)
	return absolute
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
