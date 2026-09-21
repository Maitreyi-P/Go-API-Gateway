// Command mockbackend is a small standalone HTTP server used for testing
// the gateway. It answers every request with a JSON body identifying
// itself, so it's easy to confirm which backend served a given request.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	port := flag.String("port", envOr("PORT", "9001"), "port to listen on")
	name := flag.String("name", envOr("BACKEND_NAME", "mockbackend"), "name this backend reports in its responses")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"backend": *name,
			"path":    r.URL.Path,
			"method":  r.Method,
		})
	})

	addr := ":" + *port
	logger.Info("mock backend starting", "name", *name, "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("mock backend failed", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
