package domain

import (
	"context"
	"time"
)

// Sources of an occupancy reading, in confidence order. The routing path
// (least-load dispatch) is core-computed and never reads Influx: "health" and
// "metrics" come from the gateway probing the engine directly at assignment
// time, "connections" from the gateway's own live stream accounting.
const (
	LoadSourceHealth      = "health"
	LoadSourceMetrics     = "metrics"
	LoadSourceConnections = "connections"
)

// Load is one endpoint's occupancy at one instant, as measured by the gateway.
//
// Active is the number of requests the engine has ADMITTED: processing plus
// queued in front of it. On the live fleet this is what radiance reports as
// /health "in_flight", and it is the same number as the radiance:http_queue_depth
// gauge (measured 2026-11-03: 4 concurrent generations -> in_flight=4,
// http_queue_depth=4, vllm:num_requests_running=4, vllm:num_requests_waiting=0
// - the gauge is the HTTP-layer admitted count, NOT the engine scheduler's
// waiting queue).
//
// Confident is false for the connections fallback, which counts only traffic
// this gateway proxies: an engine saturated by clients outside the gateway
// reads 0 there, so a non-confident reading ranks behind an engine that
// answered, at equal Active.
//
// A missing reading is expressed by the map entry being absent from Probe's
// result, never by Active: 0. Zero is a measurement, absence is not.
type Load struct {
	Active    int
	Source    string
	Confident bool
	SampledAt time.Time
}

// LoadProber answers "how busy is each of these endpoints, right now" for a
// set of endpoints, in one round.
//
// It exists to be called AT ASSIGNMENT TIME (on a sticky miss, when a new
// conversation needs a node), not on a timer: the answer is only worth having
// at the instant a choice is made, and a conversation already pinned to a node
// never pays for it (StickySessionWrapper.Select returns before any balancer
// runs). Implementations fan out in parallel and honour ctx: a slow engine must
// cost at most the ctx deadline and then be reported as absent, so assignment
// is delayed by the budget and never stalled by a saturated box.
//
// Implementations must not mutate endpoint status: occupancy is a routing
// signal, and grading an endpoint Busy from here is a separate decision
// (health/checker.go gradeProbeStatus) that must keep Busy a non-failure.
type LoadProber interface {
	Probe(ctx context.Context, endpoints []*Endpoint) map[string]Load
}
