// Package server exposes a local, simulation-only bootstrap API.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
)

func New(service *runtime.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]any{
			"protocol_version": protocol.Version, "status": "ok", "mode": protocol.Simulation,
		})
	})
	mux.HandleFunc("POST /v1/fixture-jobs", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request protocol.FixtureRequest
		if err := decoder.Decode(&request); err != nil {
			badJSON(w, err)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			if err == nil {
				err = errors.New("expected one JSON object")
			}
			badJSON(w, err)
			return
		}
		if err := request.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err)
			return
		}
		accepted, err := service.Submit(request)
		if err != nil {
			status, code := http.StatusServiceUnavailable, "unavailable"
			if errors.Is(err, runtime.ErrConflict) {
				status, code = http.StatusConflict, "idempotency_conflict"
			}
			writeError(w, status, code, err)
			return
		}
		write(w, http.StatusAccepted, accepted)
	})
	mux.HandleFunc("GET /v1/fixture-jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		result, err := service.Result(r.PathValue("job"))
		if err != nil {
			writeError(w, http.StatusNotFound, "job_not_found", err)
			return
		}
		write(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /v1/fixture-jobs/{job}/events", func(w http.ResponseWriter, r *http.Request) {
		after := 0
		if value := r.URL.Query().Get("after_seq"); value != "" {
			var err error
			after, err = strconv.Atoi(value)
			if err != nil || after < 0 {
				writeError(w, http.StatusBadRequest, "invalid_cursor", errors.New("after_seq must be a nonnegative integer"))
				return
			}
		}
		page, err := service.Events(r.PathValue("job"), after)
		if err != nil {
			writeError(w, http.StatusNotFound, "job_not_found", err)
			return
		}
		if after > page.NextSeq {
			writeError(w, http.StatusBadRequest, "invalid_cursor", errors.New("after_seq is ahead of this job"))
			return
		}
		write(w, http.StatusOK, page)
	})
	return mux
}

func badJSON(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	writeError(w, status, "invalid_json", err)
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	write(w, status, protocol.Error{Code: code, Message: err.Error()})
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
