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

func registerExecutionRoutes(mux *http.ServeMux, service *runtime.Service) {
	mux.HandleFunc("GET /v1/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		result, err := service.JobResult(r.PathValue("job"))
		if err != nil {
			runtimeError(w, err)
			return
		}
		write(w, 200, result)
	})
	mux.HandleFunc("GET /v1/requests/{request}/logs", func(w http.ResponseWriter, r *http.Request) {
		stream := r.URL.Query().Get("stream")
		if stream == "" {
			stream = "stdout"
		}
		offset := int64(0)
		if value := r.URL.Query().Get("offset"); value != "" {
			var err error
			offset, err = strconv.ParseInt(value, 10, 64)
			if err != nil || offset < 0 {
				writeError(w, 400, "invalid_offset", errors.New("invalid log offset"))
				return
			}
		}
		if stream != "stdout" && stream != "stderr" {
			writeError(w, 400, "invalid_stream", errors.New("stream must be stdout or stderr"))
			return
		}
		page, err := service.ReadLog(r.PathValue("request"), stream, offset)
		if err != nil {
			runtimeError(w, err)
			return
		}
		write(w, 200, page)
	})
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, service.Capabilities())
	})
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var request protocol.ExecutionRequest
		if !decodeRequest(w, r, &request) {
			return
		}
		if err := request.Validate(); err != nil {
			writeError(w, 400, "invalid_request", err)
			return
		}
		bound, err := service.SubmitExecution(request)
		if err != nil {
			runtimeError(w, err)
			return
		}
		mode := protocol.Simulation
		if request.Plan.Adapter != "fixture-v1" {
			mode = "local-process"
		}
		write(w, http.StatusAccepted, struct {
			protocol.Binding
			Mode string `json:"mode"`
		}{bound, mode})
	})
	mux.HandleFunc("GET /v1/requests/{request}", func(w http.ResponseWriter, r *http.Request) {
		result, err := service.ExecutionResult(r.PathValue("request"))
		if err != nil {
			runtimeError(w, err)
			return
		}
		write(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /v1/requests/{request}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := service.Cancel(r.PathValue("request")); err != nil {
			runtimeError(w, err)
			return
		}
		result, err := service.ExecutionResult(r.PathValue("request"))
		if err != nil {
			runtimeError(w, err)
			return
		}
		write(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /v1/jobs/{job}/events", eventHandler(service))
}

func decodeRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		badJSON(w, err)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = errors.New("expected one JSON object")
		}
		badJSON(w, err)
		return false
	}
	return true
}

func eventHandler(service *runtime.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after := 0
		limit := protocol.DefaultEventLimit
		if value := r.URL.Query().Get("after_seq"); value != "" {
			var err error
			after, err = strconv.Atoi(value)
			if err != nil || after < 0 {
				writeError(w, 400, "invalid_cursor", errors.New("after_seq must be a nonnegative integer"))
				return
			}
		}
		if value, supplied := r.URL.Query()["limit"]; supplied {
			var err error
			if len(value) != 1 {
				writeError(w, 400, "invalid_event_query", runtime.ErrEventQuery)
				return
			}
			limit, err = strconv.Atoi(value[0])
			if err != nil {
				writeError(w, 400, "invalid_event_query", runtime.ErrEventQuery)
				return
			}
		}
		page, err := service.ReadEvents(r.PathValue("job"), protocol.EventQuery{AfterSeq: after, Limit: limit})
		if err != nil {
			runtimeError(w, err)
			return
		}
		write(w, http.StatusOK, page)
	}
}

func runtimeError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "evidence_unavailable"
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, runtime.ErrConflict):
		status, code = 409, "idempotency_conflict"
	case errors.Is(err, runtime.ErrResources):
		status, code = 400, "resource_unsatisfiable"
	case errors.Is(err, runtime.ErrUnsupported):
		status, code = 422, "unsupported_contract"
	case errors.Is(err, runtime.ErrSnapshot):
		status, code = 400, "snapshot_unavailable"
	case errors.Is(err, runtime.ErrCapacity):
		status, code = 503, "capacity_exhausted"
	case errors.Is(err, runtime.ErrEventQuery):
		status, code = 400, "invalid_event_query"
	}
	writeError(w, status, code, err)
}
