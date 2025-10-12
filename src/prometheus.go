package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const ns = "drove_gateway"

var (
	countFailedReloads      prometheus.Counter
	countSuccessfulReloads  prometheus.Counter
	histogramReloadDuration prometheus.Histogram

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
	gaugeAllEndpointsDown = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "all_endpoints_down_errors",
			Help:      "1 if all endpoints for a namespace are down, 0 otherwise",
		},
		[]string{"namespace"},
	)
	countDroveAppSyncErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "drove_app_sync_failed",
			Help:      "Total number of failed Drove app syncs",
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
	histogramAppRefreshDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "drove_app_refresh_duration",
			Help:      "Drove app refresh duration",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
		},
		[]string{"namespace"},
	)

	gaugeAppsWithRoutingTag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "apps_with_routing_tag_total",
			Help:      "Total number of apps that have the configured routing tag.",
		},
		[]string{"namespace"},
	)
	gaugeAppsWithoutRoutingTag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "apps_without_routing_tag_total",
			Help:      "Total number of apps that do not have the configured routing tag.",
		},
		[]string{"namespace"},
	)
	gaugeAppsIgnoredByRealm = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "apps_ignored_by_realm_total",
			Help:      "Total number of apps ignored due to realm mismatch",
		},
		[]string{"namespace"},
	)

	haproxyAPICallsSuccessful = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "haproxy_api_calls_successful_total",
			Help:      "Total number of successful HAProxy API calls.",
		},
		[]string{"operation"},
	)
	haproxyAPICallsFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "haproxy_api_calls_failed_total",
			Help:      "Total number of failed HAProxy API calls.",
		},
		[]string{"operation"},
	)
	nginxAPICallsSuccessful = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "nginx_api_calls_successful_total",
			Help:      "Total number of successful Nginx API calls.",
		},
		[]string{"operation"},
	)
	nginxAPICallsFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "nginx_api_calls_failed_total",
			Help:      "Total number of failed Nginx API calls.",
		},
		[]string{"operation"},
	)

	gaugeConfiguredEndpoints = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "configured_endpoints_total",
			Help:      "Total number of endpoints configured for a namespace.",
		},
		[]string{"namespace"},
	)
	gaugeHealthyEndpoints = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "healthy_endpoints_total",
			Help:      "Total number of healthy endpoints for a namespace.",
		},
		[]string{"namespace"},
	)

	haproxyReconcileAllBackendsDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "haproxy_runtime_api_duration_seconds",
			Help:    "Duration of haproxyRuntimeAPI in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	nginxPlusReconcileAllBackendsDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nginx_plus_runtime_api_duration_seconds",
			Help:    "Duration of nginxPlusRuntimeAPI in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	templateRenderDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "template_render_duration_seconds",
			Help:      "Duration taken to render the config from template in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"result"},
	)
)

func setupPrometheusMetrics() {
	// Initialize metrics that depend on the proxy platform configuration.
	countFailedReloads = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reloads_failed",
			Help:      "Total number of failed " + config.ProxyPlatform + " reloads",
		},
	)
	countSuccessfulReloads = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reloads_successful",
			Help:      "Total number of successful " + config.ProxyPlatform + " reloads",
		},
	)
	histogramReloadDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "reload_duration",
			Help:      config.ProxyPlatform + " reload duration",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
		},
	)

	prometheus.MustRegister(countFailedReloads)
	prometheus.MustRegister(countSuccessfulReloads)
	prometheus.MustRegister(histogramReloadDuration)
	prometheus.MustRegister(countInvalidSubdomainLabelWarnings)
	prometheus.MustRegister(countDuplicateSubdomainLabelWarnings)
	prometheus.MustRegister(countEndpointCheckFails)
	prometheus.MustRegister(countEndpointDownErrors)
	prometheus.MustRegister(gaugeAllEndpointsDown)
	prometheus.MustRegister(countDroveAppSyncErrors)
	prometheus.MustRegister(countDroveStreamErrors)
	prometheus.MustRegister(countDroveStreamNoDataWarnings)
	prometheus.MustRegister(countDroveEventsReceived)
	prometheus.MustRegister(histogramAppRefreshDuration)
	prometheus.MustRegister(gaugeAppsWithRoutingTag)
	prometheus.MustRegister(gaugeAppsWithoutRoutingTag)
	prometheus.MustRegister(gaugeAppsIgnoredByRealm)
	prometheus.MustRegister(haproxyAPICallsSuccessful)
	prometheus.MustRegister(haproxyAPICallsFailed)
	prometheus.MustRegister(nginxAPICallsSuccessful)
	prometheus.MustRegister(nginxAPICallsFailed)
	prometheus.MustRegister(haproxyReconcileAllBackendsDuration)
	prometheus.MustRegister(nginxPlusReconcileAllBackendsDuration)
	prometheus.MustRegister(templateRenderDuration)

}

func observeReloadTimeMetric(e time.Duration) {
	histogramReloadDuration.Observe(float64(e) / float64(time.Second))
}

func observeAppRefreshTimeMetric(namespace string, e time.Duration) {
	histogramAppRefreshDuration.WithLabelValues(namespace).Observe(float64(e) / float64(time.Second))
}
