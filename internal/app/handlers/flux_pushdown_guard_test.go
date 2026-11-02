package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// fluxPushdownExceptions is the documented exception list for the pushdown
// guard below: predicate substrings that are allowed to pair a function
// call with comparison predicates, mapped to their reason. Add an entry
// ONLY when the predicate genuinely must call contains() next to
// comparisons AND the window is provably bounded - "it is cheap today" is
// not a reason (the day-window filter that burned 44.45 CPU-s was 0.67s at
// -11m too, until the window grew).
var fluxPushdownExceptions = map[string]string{}

// The pushdown anti-pattern, measured and now gated
// (PLAN-INFLUX-CPU-2026-10-23, class L1): InfluxDB's read-pushdown planner
// handles only and/or/not of COMPARISONS on _measurement/_field/tags. A
// function call anywhere inside a filter() predicate disables pushdown for
// the WHOLE filter, so a filter that compares _measurement/tags AND calls
// contains(...) turns the storage read into a full measurement scan
// (measured 18.29s wall / 44.45 CPU-s vs 0.56s / 1.62 CPU-s for one day
// window). The class is measured in CPU-seconds, so it is gated here, not
// remembered.
//
// The rule, exactly as the plan states it: FAIL a filter() predicate that
// uses contains( while ALSO comparing _measurement== or a tag equality
// (r.<tag>==... where <tag> is not an underscore column). A contains(...)
// that sits in its OWN filter stage is not on the rule: the observe legs
// render `filter(measurement equality) |> filter(contains(set:[...],
// value:r.power_target_id))`, where the stage-one comparisons still push
// down and the contains() only narrows an already-bounded read. That is
// why they need no exception entry; a stage that mixes both does.
//
// The scan is textual over the package's non-test Go sources (the Flux
// renders live in format strings; test files legitimately quote the
// anti-pattern to assert against it, so they are excluded).
var (
	fluxFilterAnchor  = regexp.MustCompile(`filter\(fn:\s*\(r\)\s*=>`)
	fluxTagEquality   = regexp.MustCompile(`r\.[a-zA-Z][a-zA-Z0-9_]*==`)
	fluxMeasurementEq = regexp.MustCompile(`_measurement==`)
)

// fluxPredicateSpan returns the text of the filter predicate opened by the
// anchor at start: from the arrow to the filter call's closing paren
// (paren-depth walk; quoted literals in these renders contain no parens).
func fluxPredicateSpan(src string, start int) string {
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return src[start:i]
			}
			depth--
		}
	}
	return src[start:]
}

func TestFluxPushdownGuardNoFunctionsInComparisonFilters(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("cannot read package dir: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, loc := range fluxFilterAnchor.FindAllStringIndex(string(src), -1) {
			predicate := fluxPredicateSpan(string(src), loc[1])
			if !strings.Contains(predicate, "contains(") {
				continue
			}
			mixesComparisons := fluxMeasurementEq.MatchString(predicate) || fluxTagEquality.MatchString(predicate)
			if !mixesComparisons {
				continue
			}
			if _, ok := fluxPushdownExceptions[predicate]; ok {
				continue
			}
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d: Flux filter compares _measurement/tags AND calls contains(): pushdown is disabled for the WHOLE filter (measured 18.29s / 44.45 CPU-s vs 0.56s / 1.62 CPU-s for one day). Split the field/tag set into (r._field==\"a\" or r._field==\"b\") / (r.endpoint==\"a\" or r.endpoint==\"b\") - see tokenDayFlux/influxBase. If a call next to comparisons is genuinely required, add the predicate to fluxPushdownExceptions with a reason.\npredicate: %s", name, line, predicate)
		}
	}
	if scanned == 0 {
		t.Fatal("the guard scanned no Go sources - it is not gating anything")
	}
	// The guard must see the FIXED renders: tokenDayFlux renders the counter
	// field as ONE equality per query and influxBase renders the endpoint set
	// as tag equalities - the rule prescribes equality, never a call.
	// carry the OR-of-equalities forms the rule prescribes.
	src, err := os.ReadFile("runtime_metrics_influx.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`r._field==%s`,
		`r.endpoint==%s`,
	} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("guard fixture drifted: runtime_metrics_influx.go no longer renders %q", want)
		}
	}
}
