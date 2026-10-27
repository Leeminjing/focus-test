package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/server"
)

func TestRequestBoundary(t *testing.T) {
	s := runtime.New()
	t.Cleanup(s.Close)
	handler := server.New(s)
	valid := `{"protocol_version":1,"idempotency_key":"test","test_ids":["fixture.pass"]}`
	cases := []struct {
		name, method, path, body string
		status                   int
	}{
		{"health", "GET", "/healthz", "", 200},
		{"submit", "POST", "/v1/fixture-jobs", valid, 202},
		{"retry", "POST", "/v1/fixture-jobs", valid, 202},
		{"conflict", "POST", "/v1/fixture-jobs", strings.Replace(valid, "fixture.pass", "fixture.fail", 1), 409},
		{"unknown-field", "POST", "/v1/fixture-jobs", strings.TrimSuffix(valid, "}") + `,"command":"echo forged"}`, 400},
		{"trailing-json", "POST", "/v1/fixture-jobs", valid + `{}`, 400},
		{"empty", "POST", "/v1/fixture-jobs", `{}`, 400},
		{"null", "POST", "/v1/fixture-jobs", `null`, 400},
		{"duplicate-case", "POST", "/v1/fixture-jobs", `{"protocol_version":1,"idempotency_key":"dup","test_ids":["fixture.pass","fixture.pass"]}`, 400},
		{"unknown-case", "POST", "/v1/fixture-jobs", strings.Replace(valid, "fixture.pass", "project.test", 1), 400},
		{"unknown-version", "POST", "/v1/fixture-jobs", strings.Replace(valid, `"protocol_version":1`, `"protocol_version":2`, 1), 400},
		{"too-large", "POST", "/v1/fixture-jobs", `{"idempotency_key":"` + strings.Repeat("x", 65<<10) + `"}`, 413},
		{"missing-job", "GET", "/v1/fixture-jobs/missing", "", 404},
		{"negative-cursor", "GET", "/v1/fixture-jobs/missing/events?after_seq=-1", "", 400},
		{"wrong-method", "DELETE", "/v1/fixture-jobs", "", 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if response.Code != tc.status {
				t.Fatalf("expected %d, got %d: %s", tc.status, response.Code, response.Body.String())
			}
			if tc.status != http.StatusMethodNotAllowed && !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatal("API response is not JSON")
			}
		})
	}
}
