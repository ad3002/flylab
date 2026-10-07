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
	"github.com/ad3002/flylab/internal/cli"
	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
	"github.com/ad3002/flylab/internal/worker"
)

const usage = `usage:
  flylab [serve]                                                    run the server
  flylab user create --username U --password P [--display-name N]   create an account
  flylab user passwd --username U --password P                      change a password`

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 0 || args[0] == "serve":
		if len(args) > 1 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		serve()
	case args[0] == "user":
		os.Exit(runUserCommand(args[1:]))
	case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s\n", args[0], usage)
		os.Exit(2)
	}
}

func runUserCommand(args []string) int {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"ok\":false,\"error\":%q}\n", err.Error())
		return 1
	}
	store, err := storage.OpenStore(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"ok\":false,\"error\":%q}\n", fmt.Sprintf("open database %s: %v", cfg.DBPath, err))
		return 1
	}
	defer store.Close()
	return cli.RunUser(args, store, os.Stdout, os.Stderr)
}

func serve() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("%v", err)
	}

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

	// Initialize the Claude planner client
	llmClient, err := llm.NewClient(cfg, validator, registry)
	if err != nil {
		log.Fatalf("Failed to initialise planner: %v", err)
	}
	if llmClient.Ready() {
		log.Printf("Planner: claude CLI %q, model %s, max %d concurrent", cfg.ClaudeBin, cfg.ClaudeModel, cfg.LLMMaxConcurrency)
	} else {
		log.Printf("WARNING: claude CLI %q not found; /plans/parse will use the keyword parser and report llm_error", cfg.ClaudeBin)
	}

	// Initialize and start background worker
	w := worker.NewWorker(cfg, store)
	if err := w.Start(); err != nil {
		// Jobs left 'running' by the previous process would otherwise stay so forever.
		log.Fatalf("Failed to start the simulation worker: %v", err)
	}
	defer w.Stop()
	log.Printf("Background simulation worker started")

	// Create API and Web Server
	srv := api.NewServer(cfg, store, validator, registry, llmClient)
	srv.SetWorker(w)

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
