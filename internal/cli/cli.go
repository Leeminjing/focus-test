// Package cli implements the command-line client against the local daemon.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"focus-test-runtime/internal/localnet"
	"focus-test-runtime/internal/protocol"
)

const DefaultAddress = "http://127.0.0.1:7337"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("focus-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("addr", DefaultAddress, "local daemon URL")
	tokenFile := flags.String("token-file", ".focus-test/token", "local runtime credential file")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: focus-test [--addr URL] [--token-file PATH] <command> [options]")
		fmt.Fprintln(stderr, "Commands: health capabilities plan submit-execution request cancel job events logs submit result snapshot manifest materialize execution-key version")
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
	switch remaining[0] {
	case "snapshot", "manifest", "materialize", "execution-key":
		return snapshotCommand(ctx, remaining[0], remaining[1:], stdout, stderr)
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
	requestID := command.String("request", "", "request ID (request/cancel)")
	adapter := command.String("adapter", "fixture-v1", "versioned adapter (plan)")
	tests := command.String("tests", "", "comma-separated test paths or fixture IDs (plan)")
	snapshotID := command.String("snapshot", "", "sealed input identity (plan)")
	caller := command.String("caller", "local", "calling context reference (plan)")
	domain := command.String("domain", "local-project", "trust domain (plan)")
	key := command.String("key", "", "idempotency key; generated if absent (plan)")
	reuse := command.String("reuse", "none", "none or inflight (plan)")
	stream := command.String("stream", "stdout", "stdout or stderr (logs)")
	offset := command.Int64("offset", 0, "byte offset (logs)")
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
	case "capabilities", "plan":
		path = "/v1/capabilities"
	case "submit", "submit-execution":
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
		if remaining[0] == "submit-execution" {
			path = "/v1/requests"
		}
	case "request", "cancel", "logs":
		if *requestID == "" {
			return errors.New("request/cancel/logs require --request")
		}
		path = "/v1/requests/" + url.PathEscape(*requestID)
		if remaining[0] == "cancel" {
			method = http.MethodPost
			path += "/cancel"
		}
		if remaining[0] == "logs" {
			if *offset < 0 {
				return errors.New("offset must be nonnegative")
			}
			path += "/logs?stream=" + url.QueryEscape(*stream) + "&offset=" + strconv.FormatInt(*offset, 10)
		}
	case "result", "events", "job":
		if *id == "" {
			return errors.New("result/events require --job")
		}
		path = "/v1/fixture-jobs/" + url.PathEscape(*id)
		if remaining[0] == "job" {
			path = "/v1/jobs/" + url.PathEscape(*id)
		}
		if remaining[0] == "events" {
			if *after < 0 {
				return errors.New("after must be nonnegative")
			}
			path = "/v1/jobs/" + url.PathEscape(*id) + "/events?after_seq=" + strconv.Itoa(*after)
		}
	default:
		return fmt.Errorf("unknown command %q", remaining[0])
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String()+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if data, err := os.ReadFile(*tokenFile); err == nil {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(data)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
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
	if remaining[0] == "plan" {
		var capabilities struct {
			Environments map[string]protocol.Environment `json:"environments"`
		}
		if err := json.Unmarshal(data, &capabilities); err != nil {
			return err
		}
		env, ok := capabilities.Environments[*adapter]
		if !ok {
			return errors.New("requested adapter is unavailable")
		}
		if *key == "" {
			*key = rand.Text()
		}
		plan := protocol.ExecutionRequest{ProtocolVersion: protocol.Version, IdempotencyKey: *key, CallerRef: *caller, TrustDomain: *domain, SnapshotID: *snapshotID,
			Plan: protocol.Plan{Adapter: *adapter, TestIDs: strings.Split(*tests, ","), Workers: 1}, Environment: env,
			Policy:    protocol.Policy{ExecutionTimeoutMS: 60000, Isolation: "local-process", Network: "declared"},
			Resources: protocol.Resources{CPUUnits: 1, MemoryMiB: 128}, RequestTimeoutMS: 90000, ReusePolicy: *reuse}
		if err := plan.Validate(); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(plan)
	}
	_, err = stdout.Write(data)
	return err
}
