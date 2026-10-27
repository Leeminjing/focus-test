package server_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"focus-test-runtime/internal/runtime"
	"focus-test-runtime/internal/server"
)

func TestOwnerCredentialBoundary(t *testing.T) {
	s := runtime.New()
	defer s.Close()
	token := strings.Repeat("a", 32)
	handler := server.Authorized(s, token)
	for _, tc := range []struct {
		path, credential string
		status           int
	}{
		{"/healthz", "", 200}, {"/v1/capabilities", "", 401}, {"/v1/capabilities", "Bearer wrong", 401}, {"/v1/capabilities", "Bearer " + token, 200},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", tc.credential)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatalf("%s: expected %d, got %d", tc.path, tc.status, out.Code)
		}
	}
}
