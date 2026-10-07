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
	"github.com/ad3002/flylab/internal/interpret"
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

	// v4: explicit neuron_ids must be neurons of the connectome. The completeness table is
	// required (the simulator cannot run without it either), so a missing file is fatal.
	neurons, err := contracts.LoadNeuronIDs(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to load the connectome neuron ids: %v", err)
	}
	validator.SetNeuronIDs(neurons)
	log.Printf("Neuron id check: %d neurons from %s", neurons.Len(), neurons.Path)

	// Open SQLite Store
	store, err := storage.OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to open SQLite store at %s: %v", cfg.DBPath, err)
	}
	defer store.Close()
	log.Printf("SQLite database opened successfully")

	// AI budget (v4): every claude -p call is recorded in llm_usage and checked against the
	// rolling 24 h global and per-account budgets.
	budget, err := llm.NewBudget(store, cfg.AIDailyBudgetUSD, cfg.AIUserDailyBudgetUSD)
	if err != nil {
		log.Fatalf("Failed to initialise the AI budget: %v", err)
	}
	log.Printf("AI budget: $%.2f per 24 h for the server, $%.2f per account", cfg.AIDailyBudgetUSD, cfg.AIUserDailyBudgetUSD)

	// Initialize the Claude planner client
	llmClient, err := llm.NewClient(cfg, validator, registry, budget)
	if err != nil {
		log.Fatalf("Failed to initialise planner: %v", err)
	}
	if llmClient.Ready() {
		log.Printf("Planner: claude CLI %q, model %s, max %d concurrent", cfg.ClaudeBin, cfg.ClaudeModel, cfg.LLMMaxConcurrency)
	} else {
		log.Printf("WARNING: claude CLI %q not found; /plans/parse will use the keyword parser and report llm_error", cfg.ClaudeBin)
	}

	// v3 interpretation: annotations (optional data file) + readout proxies (registry).
	interpreter, err := interpret.NewService(cfg, store, registry, validator, llmClient)
	if err != nil {
		log.Fatalf("Failed to initialise the interpretation service: %v", err)
	}
	if ann := interpreter.Annotations(); ann.Ready {
		log.Printf("Neuron annotations: %d neurons from %s", ann.Count(), ann.Path)
	} else {
		log.Printf("WARNING: %s not found; capabilities report annotations_ready=false and every digest carries a coverage warning (run scripts/setup_data.sh)", ann.Path)
	}

	// v4 interpretation queue: requests left running by the previous process are marked
	// failed (WORKER_INTERRUPTED); a failure to do so is fatal like a failed migration.
	if err := interpreter.Start(); err != nil {
		log.Fatalf("Failed to start the interpretation queue worker: %v", err)
	}
	defer interpreter.Stop()
	log.Printf("Interpretation queue: %d worker(s), at most %d queued; registration mode %s",
		cfg.InterpretConcurrency, cfg.InterpretQueueMax, cfg.RegistrationMode())

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
	srv.SetInterpreter(interpreter)

	httpServer := &http.Server{
		Addr:        fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:     srv.Router(),
		ReadTimeout: 30 * time.Second,
		// GET .../digest may wait for the digest slot and run flysim digest
		// (interpret.WorstCase); interpretations themselves run on the queue worker.
		WriteTimeout: interpret.WorstCase(cfg) + 60*time.Second,
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
