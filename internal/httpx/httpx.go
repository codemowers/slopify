// Package httpx runs HTTP services with environment configuration and
// graceful shutdown.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Env returns a required environment variable or exits.
func Env(name string) string {
	v := os.Getenv(name)
	if v == "" {
		slog.Error("missing required environment variable", "name", name)
		os.Exit(2)
	}
	return v
}

// EnvOr returns an environment variable or its default.
func EnvOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// SignalContext is cancelled on SIGTERM or SIGINT.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

// Serve listens on LISTEN_ADDR (default :8080) until ctx is cancelled, then
// drains in-flight requests. Request contexts derive from ctx, so long-lived
// streams such as SSE end when shutdown begins.
func Serve(ctx context.Context, h http.Handler) error {
	return ServeAddr(ctx, EnvOr("LISTEN_ADDR", ":8080"), h)
}

// ServeAddr is Serve on an explicit address.
func ServeAddr(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", srv.Addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func JSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writing response", "err", err)
	}
}
