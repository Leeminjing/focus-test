// Package server exposes a local, simulation-only bootstrap API.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"focus-test-runtime/internal/protocol"
	"focus-test-runtime/internal/runtime"
)

func New(service *runtime.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := service.Ready(); err != nil {
			runtimeError(w, err)
			return
		}
		write(w, http.StatusOK, map[string]any{
			"protocol_version": protocol.Version, "status": "ok", "mode": "local-runtime",
		})
	})
	mux.HandleFunc("POST /v1/fixture-jobs", func(w http.ResponseWriter, r *http.Request) {
		var request protocol.FixtureRequest
		if !decodeRequest(w, r, &request) {
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
	mux.HandleFunc("GET /v1/fixture-jobs/{job}/events", eventHandler(service))
	registerExecutionRoutes(mux, service)
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
