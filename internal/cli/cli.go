// Package cli implements the command-line client against the local daemon.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"focus-test-runtime/internal/localnet"
	"focus-test-runtime/internal/protocol"
)

const DefaultAddress = "http://127.0.0.1:7337"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("focus-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("addr", DefaultAddress, "local daemon URL")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: focus-test [--addr URL] <health|submit|result|events|version> [options]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		flags.Usage()
		return errors.New("a command is required")
	}
	if remaining[0] == "version" {
		if len(remaining) != 1 {
			return errors.New("version takes no arguments")
		}
		_, err := fmt.Fprintf(stdout, "focus-test-runtime dev (protocol %d)\n", protocol.Version)
		return err
	}
	u, err := url.Parse(*address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("addr must be an HTTP URL with a literal loopback IP and port")
	}
	if err := localnet.ValidateAddress(u.Host); err != nil {
		return err
	}
	u.Path = ""
	method, path := http.MethodGet, "/healthz"
	var body io.Reader
	command := flag.NewFlagSet(remaining[0], flag.ContinueOnError)
	command.SetOutput(stderr)
	file := command.String("file", "", "fixture request JSON file (submit)")
	id := command.String("job", "", "job ID (result/events)")
	after := command.Int("after", 0, "last consumed event sequence (events)")
	if err := command.Parse(remaining[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch remaining[0] {
	case "health":
	case "submit":
		if *file == "" {
			return errors.New("submit requires --file")
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if err != nil {
			return err
		}
		if len(data) > 64<<10 {
			return errors.New("fixture request exceeds 64 KiB")
		}
		method, path, body = http.MethodPost, "/v1/fixture-jobs", bytes.NewReader(data)
	case "result", "events":
		if *id == "" {
			return errors.New("result/events require --job")
		}
		path = "/v1/fixture-jobs/" + url.PathEscape(*id)
		if remaining[0] == "events" {
			if *after < 0 {
				return errors.New("after must be nonnegative")
			}
			path += "/events?after_seq=" + strconv.Itoa(*after)
		}
	default:
		return fmt.Errorf("unknown command %q", remaining[0])
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String()+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("daemon response exceeds 1 MiB")
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("daemon returned %s: %s", response.Status, bytes.TrimSpace(data))
	}
	if !json.Valid(data) {
		return errors.New("daemon returned invalid JSON")
	}
	_, err = stdout.Write(data)
	return err
}
