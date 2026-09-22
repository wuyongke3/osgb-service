package main

// main.go is the entry point. It prepares the data directories, wires the HTTP
// routes and serves until stopped. Everything it calls lives in the sibling
// files: config.go, types.go, jobs.go, pipeline.go, ply.go, api.go and util.go.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	loadDotEnv(".env")
	configuredDataDir := envOr("DATA_DIR", "./runtime")
	loadDotEnv(envOr("CONFIG_FILE", filepath.Join(configuredDataDir, "service.env")))
	config := loadConfig()
	if err := os.MkdirAll(filepath.Join(config.DataDir, "jobs"), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, directory := range []string{"uploads", "plans", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(config.DataDir, directory), 0o755); err != nil {
			log.Fatal(err)
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	manager := NewJobManager(config, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", manager.handleHealth)
	mux.HandleFunc("/api/config", manager.handleConfig)
	mux.HandleFunc("/api/pick-directory", manager.handlePickDirectory)
	mux.HandleFunc("/api/uploads", manager.handleUpload)
	mux.HandleFunc("/api/plans", manager.handlePlans)
	mux.HandleFunc("/api/plans/", manager.handlePlans)
	mux.HandleFunc("/api/jobs", manager.jobRoutes)
	mux.HandleFunc("/api/jobs/", manager.jobRoutes)
	mux.HandleFunc("/", manager.serveStatic)

	server := &http.Server{Addr: ":" + config.Port, Handler: withRequestLogging(mux, logger), ReadHeaderTimeout: 15 * time.Second}
	logger.Info("server started", "address", server.Addr, "data_dir", config.DataDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// runHealthcheck is used by Docker HEALTHCHECK. It checks the real native
// chain rather than merely confirming that the server binary can start.
func runHealthcheck() error {
	loadDotEnv(".env")
	dataDir := envOr("DATA_DIR", "./runtime")
	loadDotEnv(envOr("CONFIG_FILE", filepath.Join(dataDir, "service.env")))
	config := loadConfig()
	if strings.EqualFold(config.PipelineMode, "native") && !nativePipelineReady(config) {
		return errors.New("native photogrammetry tools are not ready")
	}
	return nil
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

func withRequestLogging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"code": code, "detail": detail})
}

// decodeLimitedJSON decodes a small JSON request body, rejecting anything
// oversized. All request bodies are small control messages, so a fixed limit
// avoids letting a client stream an unbounded payload into memory.
func decodeLimitedJSON(r *http.Request, target any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(target)
}

func writeSSE(w io.Writer, eventType string, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
}

func isSupportedImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".tif", ".tiff", ".webp":
		return true
	default:
		return false
	}
}

func validateOutputPath(outputPath, root string) error {
	if outputPath == "" {
		return errors.New("output_path is required")
	}
	clean := filepath.Clean(outputPath)
	if !filepath.IsAbs(clean) {
		return errors.New("output_path must be an absolute local path")
	}
	if clean == "." || clean == string(filepath.Separator) {
		return errors.New("output_path is not a valid file or directory path")
	}
	if root != "" {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("invalid output root: %w", err)
		}
		pathAbs, err := filepath.Abs(clean)
		if err != nil {
			return fmt.Errorf("invalid output path: %w", err)
		}
		rel, err := filepath.Rel(rootAbs, pathAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("output path must be inside OUTPUT_ROOT (%s)", rootAbs)
		}
	}
	return nil
}

// idCounter makes IDs unique even when two IDs are requested within the same
// clock tick. Job IDs also name directories on disk, so uniqueness is a hard
// requirement rather than a nicety: a collision would make two jobs share a
// workspace and corrupt each other's state.
