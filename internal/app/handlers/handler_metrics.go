package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/version"
)

func (a *Application) metricsHandler(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.gatherStatusSnapshot(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get endpoint data: %v", err), http.StatusInternalServerError)
		return
	}

	response := a.buildStatusResponse(snapshot)

	modelStats := make(map[string]ports.ModelStats)
	modelEndpointStats := make(map[string]map[string]ports.EndpointModelStats)
	if a.statsCollector != nil {
		modelStats = a.statsCollector.GetModelStats()
		modelEndpointStats = a.statsCollector.GetModelEndpointStats()
	}
	if modelStats == nil {
		modelStats = make(map[string]ports.ModelStats)
	}
	if modelEndpointStats == nil {
		modelEndpointStats = make(map[string]map[string]ports.EndpointModelStats)
	}
	models, acc := a.buildModelStats(modelStats, modelEndpointStats, false)
	summary := a.buildSummary(models, modelStats, acc)

	w.Header().Set(constants.HeaderContentType, constants.ContentTypePrometheus)
	w.WriteHeader(http.StatusOK)
	writePrometheusMetrics(w, response, snapshot, a.Config.Proxy, time.Since(a.StartTime), summary, modelStats, modelEndpointStats)
}

func writePrometheusMetrics(w http.ResponseWriter, response StatusResponse, snapshot *statusSnapshot, proxyConfig config.ProxyConfig, uptime time.Duration,
	summary ModelStatsSummary, modelStats map[string]ports.ModelStats, modelEndpointStats map[string]map[string]ports.EndpointModelStats) {
	var b strings.Builder

	writePrometheusHelpType(&b, "olla_info", "gauge", "Olla build and proxy configuration")
	writePrometheusLabeledGauge(&b, "olla_info", 1,
		"version", version.Version,
		"commit", version.Commit,
		"engine", proxyConfig.Engine,
		"profile", proxyConfig.Profile,
		"balancer", proxyConfig.LoadBalancer,
	)

	writePrometheusHelpType(&b, "olla_system_status", "gauge", "Overall system status (2=healthy, 1=degraded, 0=critical)")
	writePrometheusGauge(&b, "olla_system_status", systemStatusValue(response.System.Status))

	writePrometheusHelpType(&b, "olla_endpoints_total", "gauge", "Total configured endpoints")
	writePrometheusGauge(&b, "olla_endpoints_total", float64(len(snapshot.all)))

	writePrometheusHelpType(&b, "olla_endpoints_healthy", "gauge", "Number of healthy endpoints")
	writePrometheusGauge(&b, "olla_endpoints_healthy", float64(len(snapshot.healthy)))

	writePrometheusHelpType(&b, "olla_success_rate_percent", "gauge", "Proxy success rate percentage")
	writePrometheusGauge(&b, "olla_success_rate_percent", parsePercentage(response.System.SuccessRate))

	writePrometheusHelpType(&b, "olla_avg_latency_ms", "gauge", "Average proxy latency in milliseconds")
	writePrometheusGauge(&b, "olla_avg_latency_ms", float64(snapshot.proxyStats.AverageLatency))

	writePrometheusHelpType(&b, "olla_total_traffic_bytes", "gauge", "Total traffic across all endpoints in bytes")
	writePrometheusGauge(&b, "olla_total_traffic_bytes", float64(totalTrafficBytes(snapshot)))

	writePrometheusHelpType(&b, "olla_uptime_seconds", "gauge", "Olla process uptime in seconds")
	writePrometheusGauge(&b, "olla_uptime_seconds", uptime.Seconds())

	writePrometheusHelpType(&b, "olla_active_connections", "gauge", "Active connections across all endpoints")
	writePrometheusGauge(&b, "olla_active_connections", float64(response.System.ActiveConnections))

	writePrometheusHelpType(&b, "olla_security_violations_total", "counter", "Total security violations")
	writePrometheusGauge(&b, "olla_security_violations_total", float64(response.System.SecurityViolations))

	writePrometheusHelpType(&b, "olla_requests_total", "counter", "Total proxy requests processed")
	writePrometheusGauge(&b, "olla_requests_total", float64(response.System.TotalRequests))

	writePrometheusHelpType(&b, "olla_failures_total", "counter", "Total failed proxy requests")
	writePrometheusGauge(&b, "olla_failures_total", float64(response.System.TotalFailures))

	writePrometheusHelpType(&b, "olla_security_blocked_ips", "gauge", "Unique IPs blocked by rate limiting")
	writePrometheusGauge(&b, "olla_security_blocked_ips", float64(response.Security.BlockedIPs))

	writePrometheusHelpType(&b, "olla_security_rate_limit_violations_total", "counter", "Rate limit violations")
	writePrometheusGauge(&b, "olla_security_rate_limit_violations_total", float64(response.Security.Violations.RateLimits))

	writePrometheusHelpType(&b, "olla_security_size_limit_violations_total", "counter", "Request size limit violations")
	writePrometheusGauge(&b, "olla_security_size_limit_violations_total", float64(response.Security.Violations.SizeLimits))

	writePrometheusHelpType(&b, "olla_security_status", "gauge", "Security posture (1=normal, 0=elevated)")
	writePrometheusGauge(&b, "olla_security_status", securityStatusValue(response.Security.Status))

	writeEndpointPrometheusMetrics(&b, snapshot.all, snapshot.endpointStats, snapshot.connectionStats, snapshot.endpointModels)
	writeModelPrometheusMetrics(&b, summary, modelStats, modelEndpointStats)

	_, _ = w.Write([]byte(b.String()))
}

func writeEndpointPrometheusMetrics(b *strings.Builder, all []*domain.Endpoint, statsMap map[string]ports.EndpointStats,
	connectionStats map[string]int64, modelMap map[string]*domain.EndpointModels) {
	writePrometheusHelpType(b, "olla_endpoint_up", "gauge", "Whether the endpoint is healthy (1) or not (0)")
	writePrometheusHelpType(b, "olla_endpoint_requests_total", "counter", "Total requests handled by endpoint")
	writePrometheusHelpType(b, "olla_endpoint_connections", "gauge", "Active connections for endpoint")
	writePrometheusHelpType(b, "olla_endpoint_success_rate_percent", "gauge", "Endpoint success rate percentage")
	writePrometheusHelpType(b, "olla_endpoint_avg_latency_ms", "gauge", "Endpoint average latency in milliseconds")
	writePrometheusHelpType(b, "olla_endpoint_traffic_bytes", "counter", "Total traffic for endpoint in bytes")
	writePrometheusHelpType(b, "olla_endpoint_priority", "gauge", "Endpoint routing priority")
	writePrometheusHelpType(b, "olla_endpoint_models_count", "gauge", "Number of models discovered on endpoint")

	for _, endpoint := range all {
		url := endpoint.GetURLString()
		stats, hasStats := statsMap[url]
		status := endpoint.Status.String()

		var successRate float64
		requests := int64(0)
		avgLatency := int64(0)
		trafficBytes := int64(0)
		if hasStats {
			requests = stats.TotalRequests
			avgLatency = stats.AverageLatency
			trafficBytes = stats.TotalBytes
			if stats.TotalRequests > 0 {
				successRate = float64(stats.SuccessfulRequests) / float64(stats.TotalRequests) * 100.0
			}
		}

		modelCount := int64(0)
		if endpointModels := modelMap[url]; endpointModels != nil {
			modelCount = int64(len(endpointModels.Models))
		}

		up := float64(0)
		if endpoint.Status == domain.StatusHealthy {
			up = 1
		}

		writePrometheusLabeledGauge(b, "olla_endpoint_up", up, "endpoint", endpoint.Name, "status", status)
		writePrometheusLabeledGauge(b, "olla_endpoint_requests_total", float64(requests), "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_connections", float64(connectionStats[url]), "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_success_rate_percent", successRate, "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_avg_latency_ms", float64(avgLatency), "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_traffic_bytes", float64(trafficBytes), "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_priority", float64(endpoint.Priority), "endpoint", endpoint.Name)
		writePrometheusLabeledGauge(b, "olla_endpoint_models_count", float64(modelCount), "endpoint", endpoint.Name)
	}
}

func writeModelPrometheusMetrics(b *strings.Builder, summary ModelStatsSummary, modelStats map[string]ports.ModelStats,
	modelEndpointStats map[string]map[string]ports.EndpointModelStats) {
	var totalTrafficBytes int64
	for _, stats := range modelStats {
		totalTrafficBytes += stats.TotalBytes
	}

	writePrometheusHelpType(b, "olla_models_total", "gauge", "Total tracked models")
	writePrometheusHelpType(b, "olla_models_active", "gauge", "Models requested in the last hour")
	writePrometheusHelpType(b, "olla_models_requests_total", "counter", "Total requests across all models")
	writePrometheusHelpType(b, "olla_models_success_rate_percent", "gauge", "Overall model request success rate")
	writePrometheusHelpType(b, "olla_models_traffic_bytes", "counter", "Total model traffic in bytes")
	writePrometheusHelpType(b, "olla_model_requests_total", "counter", "Total requests for a model")
	writePrometheusHelpType(b, "olla_model_successful_requests_total", "counter", "Successful requests for a model")
	writePrometheusHelpType(b, "olla_model_failed_requests_total", "counter", "Failed requests for a model")
	writePrometheusHelpType(b, "olla_model_success_rate_percent", "gauge", "Model success rate percentage")
	writePrometheusHelpType(b, "olla_model_traffic_bytes", "counter", "Total traffic for a model in bytes")
	writePrometheusHelpType(b, "olla_model_avg_latency_ms", "gauge", "Model average latency in milliseconds")
	writePrometheusHelpType(b, "olla_model_p95_latency_ms", "gauge", "Model P95 latency in milliseconds")
	writePrometheusHelpType(b, "olla_model_p99_latency_ms", "gauge", "Model P99 latency in milliseconds")
	writePrometheusHelpType(b, "olla_model_unique_clients", "gauge", "Unique clients per model")
	writePrometheusHelpType(b, "olla_model_routing_hits_total", "counter", "Model routing hits (first endpoint)")
	writePrometheusHelpType(b, "olla_model_routing_misses_total", "counter", "Model routing misses (retry required)")
	writePrometheusHelpType(b, "olla_model_routing_fallbacks_total", "counter", "Model routing fallbacks")
	writePrometheusHelpType(b, "olla_model_endpoint_requests_total", "counter", "Requests for a model on a specific endpoint")
	writePrometheusHelpType(b, "olla_model_endpoint_success_rate_percent", "gauge", "Model success rate on a specific endpoint")
	writePrometheusHelpType(b, "olla_model_endpoint_avg_latency_ms", "gauge", "Model average latency on a specific endpoint")
	writePrometheusHelpType(b, "olla_model_endpoint_consecutive_errors", "gauge", "Consecutive errors for a model on an endpoint")

	writePrometheusGauge(b, "olla_models_total", float64(summary.TotalModels))
	writePrometheusGauge(b, "olla_models_active", float64(summary.ActiveModels))
	writePrometheusGauge(b, "olla_models_requests_total", float64(summary.TotalRequests))
	writePrometheusGauge(b, "olla_models_success_rate_percent", parsePercentage(summary.OverallSuccessRate))
	writePrometheusGauge(b, "olla_models_traffic_bytes", float64(totalTrafficBytes))

	for name, stats := range modelStats {
		successRate := float64(0)
		if stats.TotalRequests > 0 {
			successRate = float64(stats.SuccessfulRequests) / float64(stats.TotalRequests) * 100.0
		}

		writePrometheusLabeledGauge(b, "olla_model_requests_total", float64(stats.TotalRequests), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_successful_requests_total", float64(stats.SuccessfulRequests), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_failed_requests_total", float64(stats.FailedRequests), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_success_rate_percent", successRate, "model", name)
		writePrometheusLabeledGauge(b, "olla_model_traffic_bytes", float64(stats.TotalBytes), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_avg_latency_ms", float64(stats.AverageLatency), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_p95_latency_ms", float64(stats.P95Latency), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_p99_latency_ms", float64(stats.P99Latency), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_unique_clients", float64(stats.UniqueClients), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_routing_hits_total", float64(stats.RoutingHits), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_routing_misses_total", float64(stats.RoutingMisses), "model", name)
		writePrometheusLabeledGauge(b, "olla_model_routing_fallbacks_total", float64(stats.RoutingFallbacks), "model", name)
	}

	for modelName, endpoints := range modelEndpointStats {
		for epName, epStats := range endpoints {
			writePrometheusLabeledGauge(b, "olla_model_endpoint_requests_total", float64(epStats.RequestCount),
				"model", modelName, "endpoint", epName)
			writePrometheusLabeledGauge(b, "olla_model_endpoint_success_rate_percent", epStats.SuccessRate,
				"model", modelName, "endpoint", epName)
			writePrometheusLabeledGauge(b, "olla_model_endpoint_avg_latency_ms", float64(epStats.AverageLatency),
				"model", modelName, "endpoint", epName)
			writePrometheusLabeledGauge(b, "olla_model_endpoint_consecutive_errors", float64(epStats.ConsecutiveErrors),
				"model", modelName, "endpoint", epName)
		}
	}
}

func totalTrafficBytes(snapshot *statusSnapshot) int64 {
	var total int64
	for url, conn := range snapshot.connectionStats {
		_ = conn
		if stats, exists := snapshot.endpointStats[url]; exists {
			total += stats.TotalBytes
		}
	}
	return total
}

func systemStatusValue(status string) float64 {
	switch status {
	case statusHealthy:
		return 2
	case statusDegraded:
		return 1
	default:
		return 0
	}
}

func securityStatusValue(status string) float64 {
	if status == statusNormal {
		return 1
	}
	return 0
}

func parsePercentage(value string) float64 {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "%")
	var parsed float64
	_, _ = fmt.Sscanf(value, "%f", &parsed)
	return parsed
}

func writePrometheusHelpType(b *strings.Builder, name, metricType, help string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(metricType)
	b.WriteByte('\n')
}

func writePrometheusGauge(b *strings.Builder, name string, value float64) {
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(formatPrometheusFloat(value))
	b.WriteByte('\n')
}

func writePrometheusLabeledGauge(b *strings.Builder, name string, value float64, labels ...string) {
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteByte('=')
		b.WriteByte('"')
		b.WriteString(escapePrometheusLabel(labels[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	b.WriteByte(' ')
	b.WriteString(formatPrometheusFloat(value))
	b.WriteByte('\n')
}

func escapePrometheusLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func formatPrometheusFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", value), "0"), ".")
}
