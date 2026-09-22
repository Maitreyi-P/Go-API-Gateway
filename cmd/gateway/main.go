// Command gateway is the entrypoint for the reverse proxy server.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/metrics"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the gateway config file")
	port := flag.Int("port", 8080, "port for the gateway to listen on")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	m := metrics.New(registry)

	router, err := proxy.NewRouter(cfg, logger, m)
	if err != nil {
		logger.Error("failed to build router from config", "error", err)
		os.Exit(1)
	}

	for _, r := range router.Routes() {
		logger.Info("route loaded", "path_prefix", r.PathPrefix, "backends", r.Backends)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.Handle("/", router)

	addr := fmt.Sprintf(":%d", *port)
	logger.Info("gateway starting", "addr", addr, "config", *configPath, "route_count", len(router.Routes()))

	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("gateway server failed", "error", err)
		os.Exit(1)
	}
}
