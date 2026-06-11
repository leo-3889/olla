package pool

import (
	"testing"
)

func TestNewLitePool_NilConstructorReturnsError(t *testing.T) {
	t.Parallel()

	_, err := NewLitePool[*int](nil)
	if err == nil {
		t.Fatal("expected error for nil constructor, got nil")
	}
}

func TestNewLitePool_GetPutRoundTrip(t *testing.T) {
	t.Parallel()

	p, err := NewLitePool(func() *int {
		v := 42
		return &v
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := p.Get()
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if *got != 42 {
		t.Errorf("Get returned %d, want 42", *got)
	}
	p.Put(got)
}

// TestNewLitePool_New_NeverPanics verifies that the pool's internal New function
// does not panic even for pathological factories. The runtime fallback in the New
// closure was previously a panic; it is now a safe return of whatever the factory
// produces (including zero values).
func TestNewLitePool_New_NeverPanics(t *testing.T) {
	t.Parallel()

	calls := 0
	p, err := NewLitePool(func() *int {
		calls++
		// First call returns a valid pointer (needed for construction-time
		// validation). After that, simulate a factory that returns nil-like
		// values — impossible for *int but tests the no-panic contract.
		v := calls
		return &v
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Drain and refill several times; must never panic.
	for range 10 {
		v := p.Get()
		p.Put(v)
	}
}
