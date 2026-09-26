
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)


const shutdownTimeout = 10 * time.Second


type chaosSettings struct {
	FailureRate float64 `json:"failure_rate"`
	LatencyMs   int     `json:"latency_ms"`
}

func (s chaosSettings) validate() error {
	if s.FailureRate < 0 || s.FailureRate > 1 {
		return fmt.Errorf("failure_rate must be between 0.0 and 1.0, got %v", s.FailureRate)
	}
	if s.LatencyMs < 0 {
		return fmt.Errorf("latency_ms must be >= 0, got %v", s.LatencyMs)
	}
	return nil
}

type chaosState struct {
	mu       sync.RWMutex
	settings chaosSettings
}

func (c *chaosState) get() chaosSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings
}

func (c *chaosState) set(s chaosSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = s
}

func main() {
	port := flag.String("port", envOr("PORT", "9001"), "port to listen on")
	name := flag.String("name", envOr("BACKEND_NAME", "mockbackend"), "name this backend reports in its responses")
	failureRate := flag.Float64("failure-rate", 0, "probability (0.0-1.0) that a request to / returns 500 instead of 200")
	latencyMs := flag.Int("latency-ms", 0, "artificial delay, in milliseconds, added before every response")
	logFormat := flag.String("log-format", envOr("LOG_FORMAT", "json"), `log output format: "json" or "text"`)
	flag.Parse()

	logger, err := newLogger(*logFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	initial := chaosSettings{FailureRate: *failureRate, LatencyMs: *latencyMs}
	if err := initial.validate(); err != nil {
		logger.Error("invalid startup chaos settings", "error", err)
		os.Exit(1)
	}

	chaos := &chaosState{settings: initial}

	mux := http.NewServeMux()

	
	mux.HandleFunc("/chaos", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, chaos.get())

		case http.MethodPost:
			var s chaosSettings
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
			if err := s.validate(); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			chaos.set(s)
			logger.Info("chaos settings updated", "failure_rate", s.FailureRate, "latency_ms", s.LatencyMs)
			writeJSON(w, http.StatusOK, s)

		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})


	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s := chaos.get()

		if s.LatencyMs > 0 {
			time.Sleep(time.Duration(s.LatencyMs) * time.Millisecond)
		}

		if s.FailureRate > 0 && rand.Float64() < s.FailureRate {
			writeJSONError(w, http.StatusInternalServerError, "simulated failure")
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"backend": *name,
			"path":    r.URL.Path,
			"method":  r.Method,
		})
	})

	addr := ":" + *port
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("mock backend starting", "name", *name, "addr", addr,
			"failure_rate", initial.FailureRate, "latency_ms", initial.LatencyMs)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("mock backend failed", "error", err)
			os.Exit(1)
		}

	case <-ctx.Done():
		stop()
		logger.Info("shutdown signal received, draining in-flight requests", "timeout", shutdownTimeout.String())

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown timed out, forcing remaining connections closed", "error", err)
		} else {
			logger.Info("mock backend shut down cleanly")
		}
	}
}


func newLogger(format string) (*slog.Logger, error) {
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, nil)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, nil)), nil
	default:
		return nil, fmt.Errorf(`invalid -log-format %q: must be "json" or "text"`, format)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
