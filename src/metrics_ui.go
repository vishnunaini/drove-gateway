package main

import (
	_ "embed"
	"net/http"
)

//go:embed metrics_ui.html
var metricsUIPage string

// metricsUIHandler serves a lightweight in-browser dashboard that reads /v1/metrics.
func metricsUIHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = writer.Write([]byte(metricsUIPage))
}
