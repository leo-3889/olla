package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

type fakeLoad struct {
	q  float64
	ok bool
}

func (f fakeLoad) QueueDepthFor(*domain.Endpoint) (float64, bool) { return f.q, f.ok }

func TestGradeProbeStatus(t *testing.T) {
	ep := &domain.Endpoint{URLString: "http://x:1"}

	assert.Equal(t, domain.StatusBusy, gradeProbeStatus(domain.StatusHealthy, 5, fakeLoad{7, true}, ep),
		"queue at/above threshold grades a probe-healthy endpoint Busy")
	assert.Equal(t, domain.StatusHealthy, gradeProbeStatus(domain.StatusHealthy, 5, fakeLoad{4.9, true}, ep))
	assert.Equal(t, domain.StatusHealthy, gradeProbeStatus(domain.StatusHealthy, 5, fakeLoad{0, false}, ep),
		"unknown queue depth leaves the probe verdict ungraded")
	assert.Equal(t, domain.StatusHealthy, gradeProbeStatus(domain.StatusHealthy, 0, fakeLoad{999, true}, ep),
		"threshold 0 (default) disables grading entirely")
	assert.Equal(t, domain.StatusUnhealthy, gradeProbeStatus(domain.StatusUnhealthy, 5, fakeLoad{999, true}, ep),
		"grading never upgrades a failed probe")
	assert.Equal(t, domain.StatusOffline, gradeProbeStatus(domain.StatusOffline, 5, fakeLoad{999, true}, ep))
}

// TestDeadlineClient_HonorsContextBudget is the check_timeout-placebo
// regression: a shared http.Client{Timeout: 5s} kills every probe at five
// seconds no matter what check_timeout (and its context deadline) says -
// slow-but-alive engines (probe latency 5-10s under queue pressure, measured
// 2026-10-19) were declared unhealthy for a budget they were never given.
// The dispatcher must key the hard Timeout off the request deadline.
func TestDeadlineClient_HonorsContextBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	d := newDeadlineClient(&http.Client{Timeout: 500 * time.Millisecond}) // template cap BELOW the handler: only the ctx budget can save this

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slow.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := d.Do(req)
	require.NoError(t, err, "a request whose context budget exceeds the template cap must not be killed by the cap")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// And the context deadline still bites: the per-client Timeout sits ABOVE
	// it, so a request over budget fails, fast, as a deadline.
	tight, cancelTight := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelTight()
	req2, err := http.NewRequestWithContext(tight, http.MethodGet, slow.URL, http.NoBody)
	require.NoError(t, err)
	_, err = d.Do(req2)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSetLoadAwareness_DefaultOff(t *testing.T) {
	c := &HTTPHealthChecker{}
	ep := &domain.Endpoint{URLString: "http://x:1"}
	assert.Equal(t, domain.StatusHealthy, gradeProbeStatus(domain.StatusHealthy, c.busyQueueDepth, c.loadReader, ep),
		"an unarmed checker leaves Healthy verdicts untouched")
}
