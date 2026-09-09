package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityRejectsNonLoopbackHost(t *testing.T) {
	handler := Security(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "http://example/health", nil)
	request.Host = "example.com:8000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
}
func TestSecurityRejectsCrossOriginMutation(t *testing.T) {
	handler := Security(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/api/index", nil)
	request.Host = "127.0.0.1:8000"
	request.Header.Set("Origin", "http://localhost:8000")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d", response.Code)
	}
}
func TestSecurityAcceptsSameOriginAndAddsHeaders(t *testing.T) {
	handler := Security(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/api/index", nil)
	request.Host = "127.0.0.1:8000"
	request.Header.Set("Origin", "http://127.0.0.1:8000")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("faltan cabeceras de seguridad")
	}
}
