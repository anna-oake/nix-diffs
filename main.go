package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	data := flag.String("data", env("DATA_PATH", "data"), "Snapshot and diff data directory")
	addr := flag.String("listen", env("LISTEN_ADDR", "127.0.0.1:"+env("PORT", "8787")), "HTTP listen address")
	flag.Parse()
	app, err := newApp(*data, "dix")
	if err != nil {
		slog.Error("Cannot start", "error", err)
		os.Exit(1)
	}
	defer app.root.Close()
	server := &http.Server{Addr: *addr, Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	slog.Info("Listening", "address", *addr, "data", app.root.Name(), "dix", app.dix)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("Server stopped", "error", err)
		os.Exit(1)
	}
}
