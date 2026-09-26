// Command gateway is the entrypoint for the reverse proxy server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/metrics"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/proxy"
)

const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "path to the gateway config file")
	port := flag.Int("port", 8080, "port for the gateway to listen on")
	logFormat := flag.String("log-format", "json", `log output format: "json" or "text"`)
	flag.Parse()

	logger, err := newLogger(*logFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

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
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("gateway starting", "addr", addr, "config", *configPath, "route_count", len(router.Routes()))
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gateway server failed", "error", err)
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
			logger.Info("gateway shut down cleanly")
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
