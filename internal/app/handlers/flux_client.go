package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// fluxClient is a minimal InfluxDB v2 query-API client, hand-rolled so the
// proxy does not gain a heavyweight client dependency for one POST endpoint.
// The org travels as a URL query parameter deliberately: deployments that
// reach Influx through a host-published port reject bodies whose "org" is a
// name ("provide orgID or org"), while the query parameter resolves on every
// transport tried - and this client is only ever pointed at the
// docker-internal URL anyway.
type fluxClient struct {
	client  *http.Client
	baseURL string
	org     string
	token   string
}

// fluxClientTimeout bounds one Flux round trip: the metrics provider runs
// every 10s, so a wedged Influx must surface as an error, never as a stuck
// refresh loop (the write-path stall of 2026-10-12 is the cautionary tale).
const fluxClientTimeout = 5 * time.Second

func newFluxClient(baseURL, org, token string) *fluxClient {
	return &fluxClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		org:     org,
		token:   token,
		client:  &http.Client{Timeout: fluxClientTimeout},
	}
}

// query runs one Flux script and returns rows as column->value maps.
//
// The wire format is annotated CSV, deliberately: InfluxDB accepts a
// "dialect" for /api/v2/query, but deployments routinely run with server
// defaults that IGNORE a json dialect (verified against the live influxd:
// format:"json" still streams CSV) - parsing what the server actually sends
// beats trusting a capability it may not honour. The stream is one header
// row (",result,table,...") plus data rows ("_result" in the result column)
// per result table; an "error" table signals a Flux-level failure after an
// HTTP 200. Values arrive as strings; the influxFloat/influxTime helpers at
// the call sites coerce them.
func (c *fluxClient) query(ctx context.Context, flux string) ([]map[string]any, error) {
	payload, err := json.Marshal(map[string]any{
		"query":   flux,
		"type":    "flux",
		"dialect": map[string]any{"format": "csv", "header": true},
	})
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/api/v2/query?org=" + url.QueryEscape(c.org)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("influx query failed: %s: %s", resp.Status, strings.TrimSpace(string(excerpt)))
	}
	var rows []map[string]any
	reader := csv.NewReader(resp.Body)
	reader.FieldsPerRecord = -1
	reader.Comment = '#'
	var columns []string
	inErrorTable := false
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("influx query response malformed: %w", err)
		}
		// Every annotation-column row starts with the empty annotation
		// cell; anything else is not part of a result table.
		if len(record) < 2 || record[0] != "" {
			continue
		}
		switch record[1] {
		case "result":
			columns = record
			inErrorTable = false
			continue
		case "error":
			columns = record
			inErrorTable = true
			continue
		}
		if inErrorTable {
			return nil, fmt.Errorf("influx query error: %s", strings.TrimSpace(strings.Join(record[1:], " ")))
		}
		if len(columns) == 0 {
			continue
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if name == "" || i >= len(record) {
				continue
			}
			row[name] = record[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// fluxResultCacheTTL is the window in which an identical Flux text is
// served from the result cache INSTEAD of InfluxDB: one runtime refresh
// cycle (influxMetricsInterval). Four of the five per-cycle runtime legs
// scan the identical llm_runtime[-11m] window and the same texts arrive
// from the status/endpoints consumers and the observe fan-out - within a
// cycle each distinct text must hit influxd ONCE, not per caller. This
// reuses observe.go's observeTTL memo pattern, keyed by the exact Flux
// text rather than by a handler payload.
const fluxResultCacheTTL = influxMetricsInterval

// fluxResultCache wraps a fluxQuerier with per-cycle single-flight plus a
// short-TTL result cache, keyed by the exact Flux string. Rules:
//   - ONLY successful responses are cached. A failure is never cached:
//     the next caller retries immediately, and retry backoff belongs to
//     the layer that owns the leg (noteLeg, the token-day negative cache),
//     never to hidden cache state.
//   - a response whose INITIATOR context was cancelled is never cached:
//     the initiator's deadline bounds the leader's round trip, and a
//     cancelled read can be truncated - serving truncated rows for the
//     whole TTL would fabricate samples (the observe memoErr poisoning is
//     the cautionary tale).
//   - the leader runs on a context detached from its caller (deadline
//     preserved, cancellation not propagated): a browser tab closing
//     aborts its own poll, it must not kill the shared query other
//     callers are waiting on. A joiner that abandons the wait gets its
//     own ctx error and poisons nothing.
//
// The mean/max pairs stay SEPARATE queries (aggregateWindow answers one
// fn per call and merging via union() panics this influxd when _time is
// in the group key); they are cheap now - pushdown intact and the pair is
// served through this cache like everything else.
type fluxResultCache struct {
	inner fluxQuerier
	now   func() time.Time

	calls map[string]*fluxCall
	ttl   time.Duration

	mu sync.Mutex
}

// fluxCall is the single-flight record: done open means the query is
// running, closed means rows/err are final; cacheable means success with
// a live initiator at completion. Fields are written strictly BEFORE
// close(done); joiners read them only after done, so no lock is needed
// on the result path.
type fluxCall struct {
	at        time.Time
	err       error
	done      chan struct{}
	rows      []map[string]any
	cacheable bool
}

func newFluxResultCache(inner fluxQuerier, ttl time.Duration) *fluxResultCache {
	return &fluxResultCache{inner: inner, ttl: ttl, now: time.Now, calls: map[string]*fluxCall{}}
}

func (c *fluxResultCache) query(ctx context.Context, flux string) ([]map[string]any, error) {
	c.mu.Lock()
	now := c.now()
	for key, call := range c.calls {
		select {
		case <-call.done:
			if !call.cacheable || now.Sub(call.at) > c.ttl {
				delete(c.calls, key)
			}
		default: // in flight: joiners attach to it
		}
	}
	call, joined := c.calls[flux]
	if !joined {
		call = &fluxCall{done: make(chan struct{})}
		c.calls[flux] = call
	}
	c.mu.Unlock()

	if joined {
		select {
		case <-call.done:
			return call.rows, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	lead, cancel := detachedCopyWithDeadline(ctx)
	rows, err := c.inner.query(lead, flux)
	cancel()
	call.rows, call.err = rows, err
	call.cacheable = err == nil && ctx.Err() == nil
	call.at = c.now()
	close(call.done)
	return rows, err
}

// detachedCopyWithDeadline keeps the initiating context's DEADLINE (that
// is the real bound on the leader's round trip: the 5s interactive cap,
// the 60s token-day budget - the cache must not smuggle a query past the
// deadline semantics of its caller) and drops its cancellation, so one
// caller's disconnect cannot kill the query other callers share.
func detachedCopyWithDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok {
		return context.WithTimeout(base, time.Until(dl))
	}
	return base, func() {}
}
