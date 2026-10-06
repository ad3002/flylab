package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ad3002/flylab/internal/api"
	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
	"github.com/ad3002/flylab/internal/worker"
)

func main() {
	cfg := config.LoadConfig()

	log.Printf("==================================================================")
	log.Printf("FlyLab - Drosophila Connectome Simulation Platform")
	log.Printf("Host: %s:%d | Domain: %s", cfg.Host, cfg.Port, cfg.Domain)
	log.Printf("Data dir: %s | DB: %s", cfg.DataDir, cfg.DBPath)
	log.Printf("==================================================================")

	// Load Registry
	registry, err := contracts.LoadRegistry(cfg.RegistryDir)
	if err != nil {
		log.Fatalf("Failed to load neuron groups registry from %s: %v", cfg.RegistryDir, err)
	}
	log.Printf("Loaded %d registered neuron groups", len(registry.Groups))

	// Load Schema Validator
	validator, err := contracts.NewValidator(cfg.ContractsDir, registry)
	if err != nil {
		log.Fatalf("Failed to load schema validator from %s: %v", cfg.ContractsDir, err)
	}
	log.Printf("Loaded experiment schema validator")

	// Open SQLite Store
	store, err := storage.OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to open SQLite store at %s: %v", cfg.DBPath, err)
	}
	defer store.Close()
	log.Printf("SQLite database opened successfully")

	// Initialize LLM Client
	llmClient := llm.NewClient(cfg, validator, registry)

	// Initialize and start background worker
	w := worker.NewWorker(cfg, store)
	w.Start()
	defer w.Stop()
	log.Printf("Background simulation worker started")

	// Create API and Web Server
	srv := api.NewServer(cfg, store, validator, registry, llmClient)

	httpServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      srv.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("FlyLab Web UI & API listening at http://%s:%d (Domain: %s)", cfg.Host, cfg.Port, cfg.Domain)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failure: %v", err)
		}
	}()

	<-stopChan
	log.Printf("Shutting down FlyLab gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	log.Printf("FlyLab shutdown completed.")
}
