package health

import (
	"context"
	"testing"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
)

func testThresholds() config.SuccessRateThresholds {
	return config.SuccessRateThresholds{CriticalBelow: 90, DegradedBelow: 95}
}

// mkEndpoints builds n endpoints, the first k of them healthy.
func mkEndpoints(n, healthyCount int) []*domain.Endpoint {
	endpoints := make([]*domain.Endpoint, 0, n)
	for i := 0; i < n; i++ {
		status := domain.StatusHealthy
		if i >= healthyCount {
			status = domain.StatusUnhealthy
		}
		endpoints = append(endpoints, &domain.Endpoint{
			Name:      "e",
			URLString: "http://e:11434",
			Status:    status,
		})
	}
	return endpoints
}

func hostInput(all, healthy []*domain.Endpoint, proxy ports.ProxyStats) *Input {
	return &Input{
		All:        all,
		Healthy:    healthy,
		ProxyStats: proxy,
		Thresholds: testThresholds(),
	}
}

func TestHostEvaluator_AllHealthyNoTraffic(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 3) // all three healthy
	res := e.Evaluate(context.Background(), hostInput(all, all, ports.ProxyStats{}))
	if res.Status != StatusHealthy {
		t.Errorf("all healthy, no traffic: got %q, want healthy", res.Status)
	}
	if res.Scoped {
		t.Error("host mode must not be scoped")
	}
}

func TestHostEvaluator_PartialHealthyNoTraffic_Degraded(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 2) // 2/3 healthy = 0.667, in [0.5, 0.8)
	res := e.Evaluate(context.Background(), hostInput(all, all[:2], ports.ProxyStats{}))
	if res.Status != StatusDegraded {
		t.Errorf("2/3 healthy, no traffic: got %q, want degraded", res.Status)
	}
}

func TestHostEvaluator_MostlyDownNoTraffic_Critical(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 1) // 1/3 healthy = 0.333 < 0.5
	res := e.Evaluate(context.Background(), hostInput(all, all[:1], ports.ProxyStats{}))
	if res.Status != StatusCritical {
		t.Errorf("1/3 healthy, no traffic: got %q, want critical", res.Status)
	}
}

func TestHostEvaluator_AllHealthyLowSuccessRate_Critical(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 3)
	proxy := ports.ProxyStats{TotalRequests: 100, SuccessfulRequests: 80} // 80% < 90
	res := e.Evaluate(context.Background(), hostInput(all, all, proxy))
	if res.Status != StatusCritical {
		t.Errorf("all healthy but 80%% success: got %q, want critical", res.Status)
	}
}

func TestHostEvaluator_AllHealthyMidSuccessRate_Degraded(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 3)
	proxy := ports.ProxyStats{TotalRequests: 100, SuccessfulRequests: 92} // 92% in [90,95)
	res := e.Evaluate(context.Background(), hostInput(all, all, proxy))
	if res.Status != StatusDegraded {
		t.Errorf("all healthy but 92%% success: got %q, want degraded", res.Status)
	}
}

func TestHostEvaluator_AllHealthyHighSuccessRate_Healthy(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 3)
	proxy := ports.ProxyStats{TotalRequests: 100, SuccessfulRequests: 99} // 99% >= 95
	res := e.Evaluate(context.Background(), hostInput(all, all, proxy))
	if res.Status != StatusHealthy {
		t.Errorf("all healthy and 99%% success: got %q, want healthy", res.Status)
	}
}

func TestHostEvaluator_ZeroEndpoints_Critical(t *testing.T) {
	e := NewHostEvaluator()
	res := e.Evaluate(context.Background(), hostInput(nil, nil, ports.ProxyStats{}))
	if res.Status != StatusCritical {
		t.Errorf("zero endpoints: got %q, want critical", res.Status)
	}
}

func TestHostEvaluator_CustomThresholds(t *testing.T) {
	e := NewHostEvaluator()
	all := mkEndpoints(3, 3)
	// 92% success is healthy under the default 90/95, but with a stricter
	// 95/99 it must be degraded.
	in := hostInput(all, all, ports.ProxyStats{TotalRequests: 100, SuccessfulRequests: 92})
	in.Thresholds = config.SuccessRateThresholds{CriticalBelow: 95, DegradedBelow: 99}
	res := e.Evaluate(context.Background(), in)
	if res.Status != StatusDegraded {
		t.Errorf("92%% success with 95/99 thresholds: got %q, want degraded", res.Status)
	}
}
