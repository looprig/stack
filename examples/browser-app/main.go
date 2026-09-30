// Command browser-app is a single-process, multi-user Looprig web app built
// on github.com/looprig/stack: Factory, an in-process Host running a harness
// agent, local durable storage and dev-only sign-in.
//
//	go run ./examples/browser-app
//
// It runs a scripted, keyless model (inference/inferencetest). Put a real
// provider's inference.Client and model.Model in Config instead.
//
// If you add sandboxed tools (Bash through github.com/looprig/sandbox), call
// sandbox.Init() as the very first statement of main: on Linux the sandbox
// refuses to build an executor without it. This agent runs no commands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/looprig/inference/inferencetest"
	"github.com/looprig/inference/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "browser-app:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := envOr("LOOPRIG_ADDR", "127.0.0.1:8080")
	dataDir, err := filepath.Abs(envOr("LOOPRIG_DATA", "data"))
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app, dev, err := Start(ctx, Config{
		DataDir:  dataDir,
		Tenant:   "dev",
		DevToken: os.Getenv("LOOPRIG_DEV_TOKEN"),
		Origins:  []string{"http://localhost:" + port, "http://127.0.0.1:" + port},
		Client:   scriptedModel(),
		Model:    inferencetest.Model(model.WithTools(), model.WithThinking()),
		Logger:   logger,
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	fmt.Printf("browser-app listening on http://%s (data in %s)\n", addr, dataDir)
	fmt.Printf("DEV sign-in: http://%s/dev/login, any username, token %s\n", addr, dev.Token())
	fmt.Printf("curl -H 'Authorization: Bearer %s' http://%s/v1/sessions\n", dev.Credential("alice"), addr)

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "browser-app: listener:", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Stop the stack first: it fences new commands and closes browser links
	// while the listener is still up, then drains the Host.
	return errors.Join(app.Stop(shutdown), server.Shutdown(shutdown))
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
