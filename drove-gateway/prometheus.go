package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const ns = "drove-gateway"

var (
	countFailedReloads = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reloads_failed",
			Help:      "Total number of failed Nginx reloads",
		},
	)
	countSuccessfulReloads = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reloads_successful",
			Help:      "Total number of successful Nginx reloads",
		},
	)
	histogramReloadDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "reload_duration",
			Help:      "Nginx reload duration",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
		},
	)
	countInvalidSubdomainLabelWarnings = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "invalid_subdomain_label_warnings",
			Help:      "Total number of warnings about invalid subdomain label",
		},
	)
	countDuplicateSubdomainLabelWarnings = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "duplicate_subdomain_label_warnings",
			Help:      "Total number of warnings about duplicate subdomain label",
		},
		[]string{"namespace"},
	)
	countEndpointCheckFails = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "endpoint_check_fails",
			Help:      "Total number of endpoint check failure errors",
		},
		[]string{"namespace"},
	)
	countEndpointDownErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "endpoint_down_errors",
			Help:      "Total number of endpoint down errors",
		},
		[]string{"namespace"},
	)
	countAllEndpointsDownErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "all_endpoints_down_errors",
			Help:      "Total number of all endpoints down errors",
		},
		[]string{"namespace"},
	)
	countDroveAppSyncErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "drove_app_sync_failed",
			Help:      "Total number of failed Nginx reloads",
		},
		[]string{"namespace"},
	)
	countDroveStreamErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "drove_stream_errors",
			Help:      "Total number of Drove stream errors",
		},
		[]string{"namespace"},
	)
	countDroveStreamNoDataWarnings = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "drove_stream_no_data_warnings",
			Help:      "Total number of warnings about no data in Drove stream",
		},
		[]string{"namespace"},
	)
	countDroveEventsReceived = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "drove_events_received",
			Help:      "Total number of received Drove events",
		},
		[]string{"namespace"},
	)
	histogramAppRefereshDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "drove_app_referesh_duration",
			Help:      "Drove app referesh duration",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
		},
		[]string{"namespace"},
	)
)

func setupPrometheusMetrics() {
	prometheus.MustRegister(countFailedReloads)
	prometheus.MustRegister(countSuccessfulReloads)
	prometheus.MustRegister(histogramReloadDuration)
	prometheus.MustRegister(countInvalidSubdomainLabelWarnings)
	prometheus.MustRegister(countDuplicateSubdomainLabelWarnings)
	prometheus.MustRegister(countEndpointCheckFails)
	prometheus.MustRegister(countEndpointDownErrors)
	prometheus.MustRegister(countAllEndpointsDownErrors)
	prometheus.MustRegister(countDroveAppSyncErrors)
	prometheus.MustRegister(countDroveStreamErrors)
	prometheus.MustRegister(countDroveStreamNoDataWarnings)
	prometheus.MustRegister(countDroveEventsReceived)
	prometheus.MustRegister(histogramAppRefereshDuration)

}

func observeReloadTimeMetric(e time.Duration) {
	histogramReloadDuration.Observe(float64(e) / float64(time.Second))
}

func observeAppRefreshTimeMetric(namesapece string, e time.Duration) {
	histogramAppRefereshDuration.WithLabelValues(namesapece).Observe(float64(e) / float64(time.Second))
}
