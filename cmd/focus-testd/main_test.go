package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDaemonStartupAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	dataDir := t.TempDir()
	go func() {
		defer writer.Close()
		done <- run(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", dataDir, "--adapter-dir", "../../adapters"}, writer, io.Discard)
	}()
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		t.Fatalf("daemon did not announce address: %v", <-done)
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 4 {
		t.Fatalf("invalid startup message: %q", scanner.Text())
	}
	address := fields[3]
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(address + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health returned %s", response.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", u.Host, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("daemon listener remained open after shutdown")
	}
}
