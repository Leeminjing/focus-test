package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/snapshot"
)

func snapshotCommand(ctx context.Context, name string, args []string, out, stderr io.Writer) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	storePath := flags.String("store", ".focus-test/snapshots", "private snapshot store directory")
	source := flags.String("source", ".", "source directory to seal")
	id := flags.String("snapshot", "", "sealed snapshot identity")
	exclude := flags.String("exclude", "", "comma-separated relative paths to exclude")
	file := flags.String("file", "", "execution request JSON (execution-key)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if name == "execution-key" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
		decoder.DisallowUnknownFields()
		var request protocol.ExecutionRequest
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("expected one execution request JSON object")
		}
		key, err := request.ExecutionKey()
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]string{"execution_key": key, "verification": "declared_inputs_only"})
	}
	store, err := snapshot.Open(*storePath)
	if err != nil {
		return err
	}
	defer store.Close()
	switch name {
	case "snapshot":
		var excludes []string
		if *exclude != "" {
			excludes = strings.Split(*exclude, ",")
		}
		sealed, err := store.Seal(ctx, *source, excludes)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(sealed)
	case "manifest":
		sealed, err := store.Load(*id)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(sealed)
	case "materialize":
		directory, err := store.Materialize(ctx, *id)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]string{"snapshot_id": *id, "workspace": directory})
	}
	return errors.New("unknown snapshot operation")
}
