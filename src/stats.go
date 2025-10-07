package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/peterbourgon/g2s"
)

func setupStatsd() (g2s.Statter, error) {
	if config.Statsd.Addr == "" {
		return g2s.Noop(), nil
	}

	if config.Statsd.Namespace == "" {
		hostname, _ := os.Hostname()
		config.Statsd.Namespace = "drove_gateway." + hostname
	}

	if config.Statsd.SampleRate < 1 || config.Statsd.SampleRate > 100 {
		config.Statsd.SampleRate = 100
	}

	return g2s.Dial("udp", config.Statsd.Addr)
}

func statsCount(metric string, n int) {
	ns := config.Statsd.Namespace
	statsd.Counter(1.0, ns+"."+metric, n)
}

func statsTiming(metric string, elapsed time.Duration) {
	ns := config.Statsd.Namespace
	statsd.Timing(1.0, ns+"."+metric, elapsed)
}

// statsCountVec sends a counter metric with labels.
// Labels are appended to the metric name, separated by dots.
func statsCountVec(metric string, n int, labels ...string) {
	ns := config.Statsd.Namespace
	fullMetric := strings.Join(append([]string{ns, metric}, labels...), ".")
	statsd.Counter(1.0, fullMetric, n)
}

// statsGaugeVec sends a gauge metric with labels.
func statsGaugeVec(metric string, value float64, labels ...string) {
	ns := config.Statsd.Namespace
	fullMetric := strings.Join(append([]string{ns, metric}, labels...), ".")
	statsd.Gauge(1.0, fullMetric, fmt.Sprintf("%f", value))
}

// statsTimingVec sends a timing metric with labels.
// Labels are appended to the metric name, separated by dots.
func statsTimingVec(metric string, elapsed time.Duration, labels ...string) {
	ns := config.Statsd.Namespace
	fullMetric := strings.Join(append([]string{ns, metric}, labels...), ".")
	statsd.Timing(1.0, fullMetric, elapsed)
}
