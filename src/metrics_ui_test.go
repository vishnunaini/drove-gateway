package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsUIHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/ui/metrics", nil)
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
	if !strings.Contains(body, "data-panel-slider=\"span\"") {
		t.Fatalf("body does not contain width slider control")
	}
	if !strings.Contains(body, "data-panel-slider=\"height\"") {
		t.Fatalf("body does not contain height slider control")
	}
	if !strings.Contains(body, "series-table-wrap") {
		t.Fatalf("body does not contain internal series table scroll wrapper")
	}
}
