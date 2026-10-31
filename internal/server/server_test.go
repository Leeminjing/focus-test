package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"focus-test-runtime/internal/protocol"
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

func TestEventPaginationOverHTTP(t *testing.T) {
	s := runtime.New()
	bound, err := s.Submit(protocol.FixtureRequest{ProtocolVersion: 1, IdempotencyKey: "pages", TestIDs: []string{"fixture.pass"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	handler := server.New(s)
	after := 0
	for {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", fmt.Sprintf("/v1/jobs/%s/events?after_seq=%d&limit=1", bound.JobID, after), nil))
		var page protocol.EventPage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("bad page: %s", response.Body.String())
		}
		if len(page.Events) != 1 || page.NextSeq != after+1 {
			t.Fatalf("invalid continuation: %+v", page)
		}
		after = page.NextSeq
		if page.Terminal {
			if page.HasMore {
				t.Fatal("terminal has tail")
			}
			break
		}
		if !page.HasMore {
			t.Fatal("closed job lost events")
		}
	}
	for _, query := range []string{"limit=0", "limit=257", "limit=NaN", "limit=1&limit=2", "after_seq=99999"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/jobs/"+bound.JobID+"/events?"+query, nil))
		if response.Code != 400 {
			t.Fatalf("query %s returned %d", query, response.Code)
		}
	}
}

func TestEvidenceReferenceRoutes(t *testing.T) {
	s := runtime.New()
	defer s.Close()
	b, err := s.Submit(protocol.FixtureRequest{ProtocolVersion: 1, IdempotencyKey: "retain", TestIDs: []string{"fixture.slow"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Authorized(s, strings.Repeat("a", 32))
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/jobs/" + b.JobID + "/references", `{"reference_id":"focus-adoption-A"}`, 200},
		{"POST", "/v1/jobs/" + b.JobID + "/references", `{"reference_id":"focus-adoption-A"}`, 200},
		{"POST", "/v1/jobs/" + b.JobID + "/references", `{"reference_id":" "}`, 400},
		{"DELETE", "/v1/jobs/" + b.JobID + "/references/focus-adoption-A", "", 200},
		{"POST", "/v1/jobs/unknown/references", `{"reference_id":"focus-adoption-A"}`, 404},
		{"POST", "/v1/maintenance/collect", "", 200},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
}
