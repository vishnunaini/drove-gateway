package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsUIHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/metrics/ui", nil)
	response := httptest.NewRecorder()

	metricsUIHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", response.Code, http.StatusOK)
	}

	contentType := response.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", contentType)
	}

	body := response.Body.String()
	if !strings.Contains(body, "/v1/metrics") {
		t.Fatalf("body does not contain metrics endpoint reference")
	}
	if !strings.Contains(body, "Drove Gateway Metrics UI") {
		t.Fatalf("body does not contain page title")
	}
	if !strings.Contains(body, "Native/sparse histograms") {
		t.Fatalf("body does not contain native histogram compatibility note")
	}
	if !strings.Contains(body, "delta sum / delta count") {
		t.Fatalf("body does not contain interval histogram calculation")
	}
}
