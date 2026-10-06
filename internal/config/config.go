package config

import (
	"os"
	"strconv"
)

type Config struct {
	Host             string
	Port             int
	Domain           string
	DBPath           string
	DataDir          string
	ArtifactsDir     string
	RegistryDir      string
	ContractsDir     string
	WebDir           string
	FlysimBin        string
	OllamaURL        string
	OllamaModel      string
	MaxWallSeconds   int
	MaxRSSBytes      int64
	MaxArtifactBytes int64
}

func LoadConfig() *Config {
	port := 8080
	if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = p
	}

	maxWall := 3600
	if w, err := strconv.Atoi(os.Getenv("MAX_WALL_SECONDS")); err == nil {
		maxWall = w
	}

	return &Config{
		Host:             getEnv("HOST", "0.0.0.0"),
		Port:             port,
		Domain:           getEnv("DOMAIN", "flylab.aglabx.com"),
		DBPath:           getEnv("DB_PATH", "flylab.db"),
		DataDir:          getEnv("DATA_DIR", "data"),
		ArtifactsDir:     getEnv("ARTIFACTS_DIR", "artifacts"),
		RegistryDir:      getEnv("REGISTRY_DIR", "registry"),
		ContractsDir:     getEnv("CONTRACTS_DIR", "contracts"),
		WebDir:           getEnv("WEB_DIR", "web"),
		FlysimBin:        getEnv("FLYSIM_BIN", "bin/flysim"),
		OllamaURL:        getEnv("OLLAMA_URL", "http://127.0.0.1:11434"),
		OllamaModel:      getEnv("OLLAMA_MODEL", "qwen3:8b"),
		MaxWallSeconds:   maxWall,
		MaxRSSBytes:      24 * 1024 * 1024 * 1024, // 24 GB
		MaxArtifactBytes: 2 * 1024 * 1024 * 1024,  // 2 GB
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
