package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"obsigon/internal/config"
	"obsigon/internal/db"
	"obsigon/internal/httpapi"
	"obsigon/internal/ollama"
	"obsigon/internal/rag"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	showVersion := flag.Bool("version", false, "muestra la versión y termina")
	flag.Parse()
	if *showVersion {
		fmt.Printf("obsigon %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	baseDir := resolveBaseDir(workingDir)
	settings, err := config.Load(baseDir)
	if err != nil {
		return fmt.Errorf("configuración no válida: %w", err)
	}
	store, err := db.Open(settings.DatabasePath)
	if err != nil {
		return fmt.Errorf("no se pudo abrir SQLite: %w", err)
	}
	defer store.Close()
	client := ollama.New(settings.OllamaBaseURL, settings.ChatModel, settings.EmbeddingModel, settings.OllamaKeepAlive, time.Duration(settings.OllamaTimeoutSeconds*float64(time.Second)), settings.EmbeddingWorkers)
	indexer := &rag.Indexer{Settings: settings, Store: store, Embedder: client}
	vectors := rag.NewVectorStore(store, settings.EmbeddingWorkers, rag.ANNConfig{Enabled: settings.ANNEnabled, Path: settings.DatabasePath + ".hnsw", M: settings.ANNM, EFConstruction: settings.ANNEFConstruction, EFSearch: settings.ANNEFSearch})
	reranker := rag.NewNoopReranker()
	switch settings.RerankBackend {
	case "llm":
		reranker = rag.NewLLMReranker(client, settings.RerankEnabled, settings.RerankCandidates, settings.RerankWeight)
	case "cross_encoder":
		reranker = rag.NewCrossEncoderReranker(settings.CrossEncoderURL, settings.CrossEncoderModel, settings.RerankEnabled, settings.RerankCandidates, settings.RerankWeight, time.Duration(settings.CrossEncoderTimeoutSeconds*float64(time.Second)))
	}
	retriever := &rag.Retriever{
		Settings: settings,
		Store:    store,
		Vectors:  vectors,
		Embedder: client,
		Reranker: reranker,
	}
	memory := &rag.MemoryManager{Settings: settings, Store: store, Generator: client}
	server := &httpapi.Server{Settings: settings, Store: store, Indexer: indexer, Retriever: retriever, Generator: client, Memory: memory, Progress: httpapi.NewIndexProgress(), Logger: logger, Build: httpapi.BuildInfo{Version: version, Commit: commit, Date: date}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if settings.WatchEnabled {
		server.Watcher = rag.NewVaultWatcher(settings.VaultPath, indexer, time.Duration(settings.WatchDebounceMS)*time.Millisecond, time.Duration(settings.WatchReconcileIntervalMS)*time.Millisecond)
		go server.Watcher.Run(ctx)
	}
	address := net.JoinHostPort(settings.AppHost, fmt.Sprint(settings.AppPort))
	return httpapi.Listen(ctx, address, server.Handler(), logger, settings)
}

// resolveBaseDir permite ejecutar `go run .` desde cmd/server sin convertir
// esa subcarpeta en la raíz de configuración. Fuera del árbol de fuentes se
// conserva el directorio de trabajo, que es el comportamiento esperado para
// un binario distribuido junto a su .env.
func resolveBaseDir(start string) string {
	resolved, err := filepath.Abs(start)
	if err != nil {
		return filepath.Clean(start)
	}
	for candidate := resolved; ; candidate = filepath.Dir(candidate) {
		data, readErr := os.ReadFile(filepath.Join(candidate, "go.mod"))
		if readErr == nil && isObsigonModule(data) {
			return candidate
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return resolved
}

func isObsigonModule(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "module obsigon" {
			return true
		}
	}
	return false
}
