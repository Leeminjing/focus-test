package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"focus-test-runtime/internal/localnet"
	"focus-test-runtime/internal/runner"
	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/server"
	"focus-test-runtime/internal/snapshot"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("focus-testd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("listen", "127.0.0.1:7337", "literal loopback IP and port; 0 chooses a free port")
	dataDir := flags.String("data-dir", ".focus-test", "private runtime state and snapshot directory")
	cpu := flags.Int("cpu-units", 2, "CPU admission budget")
	memory := flags.Int("memory-mib", 1024, "memory admission budget")
	adapterDir := flags.String("adapter-dir", "adapters", "versioned local adapter script directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := localnet.ValidateAddress(*address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	store, err := snapshot.Open(filepath.Join(*dataDir, "snapshots"))
	if err != nil {
		return err
	}
	defer store.Close()
	provider, err := runner.Open(*adapterDir)
	if err != nil {
		return err
	}
	service, err := runtime.Open(runtime.Config{Snapshots: store, StateDir: filepath.Join(*dataDir, "state"), CPUUnits: *cpu, MemoryMiB: *memory, Runner: provider})
	if err != nil {
		return err
	}
	defer service.Close()
	token, err := localnet.EnsureToken(filepath.Join(*dataDir, "token"))
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler: server.Authorized(service, token), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	if _, err := fmt.Fprintf(stdout, "focus-testd listening on http://%s (local runtime)\n", listener.Addr()); err != nil {
		_ = httpServer.Close()
		<-done
		return err
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
			<-done
			return err
		}
		<-done
		return nil
	}
}
