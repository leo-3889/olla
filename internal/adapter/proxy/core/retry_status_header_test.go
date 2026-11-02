package core

import (
	"net/http/httptest"
	"testing"
)

// TestStatusGateWriter_DiscardResetsHeaderMap pins MAJOR-2: discard() must
// clear the underlying writer's Header() map so a rotated-away attempt's
// response headers don't leak into the next attempt's response.
func TestStatusGateWriter_DiscardResetsHeaderMap(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newStatusGate(rec, map[int]struct{}{503: {}})

	// Simulate proxyToSingleEndpoint: headers are set on w.Header() before WriteHeader
	g.Header().Set("X-Refused", "yes")
	g.Header().Set("Content-Type", "application/x-ndjson")
	g.WriteHeader(503) // gate intercepts, holds
	_, _ = g.Write([]byte(`{"error":"no"}`))

	// Header was set on the underlying writer (Header() forwards to next)
	if rec.Header().Get("X-Refused") != "yes" {
		t.Fatal("header should be set on underlying writer before discard")
	}

	g.discard()

	if rec.Header().Get("X-Refused") != "" {
		t.Errorf("MAJOR-2: discard() must reset the header map, got X-Refused=%q",
			rec.Header().Get("X-Refused"))
	}
}

// TestStatusGateWriter_DiscardKeepsPreProxyHeaders pins the rebase regression:
// the translation handler writes X-Olla-Mode on the response writer before the
// proxy runs, so discarding a rotated-away refusal must restore the pre-proxy
// header set rather than wipe the map.
func TestStatusGateWriter_DiscardKeepsPreProxyHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Olla-Mode", "passthrough")
	g := newStatusGate(rec, map[int]struct{}{503: {}})

	g.Header().Set("X-Refused", "yes")
	g.WriteHeader(503)
	_, _ = g.Write([]byte(`{"error":"no"}`))

	g.discard()

	if got := rec.Header().Get("X-Olla-Mode"); got != "passthrough" {
		t.Errorf("discard() erased a header the handler set before the proxy ran: %q", got)
	}
	if rec.Header().Get("X-Refused") != "" {
		t.Errorf("MAJOR-2: refused attempt's header leaked: %q", rec.Header().Get("X-Refused"))
	}
}
