package pool

import (
	"sync"
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

	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get returned unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if *got != 42 {
		t.Errorf("Get returned %d, want 42", *got)
	}
	p.Put(got)
}

// TestLitePool_IsNilValue verifies that isNilValue correctly identifies typed and untyped nils.
func TestLitePool_IsNilValue(t *testing.T) {
	t.Parallel()

	// A (*int)(nil) stored in an interface{} is not == nil at the interface level.
	var typedNil *int
	if !isNilValue(typedNil) {
		t.Fatal("isNilValue must return true for a typed nil pointer")
	}

	// Non-nil pointer must not be detected as nil.
	v := 1
	if isNilValue(&v) {
		t.Fatal("isNilValue must return false for a non-nil pointer")
	}
}

// TestLitePool_Get_ErrorOnNilFactory verifies that Get returns a non-nil error (not a panic)
// when a stateful factory violates its contract and returns nil after construction.
// We cannot replicate this via NewLitePool (it validates at construction time), so we
// construct a Pool directly with a sync.Pool whose New always yields a typed nil.
func TestLitePool_Get_ErrorOnNilFactory(t *testing.T) {
	t.Parallel()

	// Bypass NewLitePool's construction-time guard so we can exercise the Get() error path.
	p := &Pool[*int]{
		pool: sync.Pool{
			New: func() any {
				// Return a typed nil (*int)(nil) - not the same as untyped nil in an
				// interface, which is what isNilValue is specifically designed to catch.
				var n *int
				return n
			},
		},
	}

	got, err := p.Get()
	if err == nil {
		t.Fatal("expected error when factory returns nil, got nil error")
	}
	if got != nil {
		t.Errorf("expected zero value on error, got non-nil: %v", got)
	}
	const wantMsg = "litepool: factory returned nil"
	if err.Error() != wantMsg {
		t.Errorf("unexpected error message: got %q, want %q", err.Error(), wantMsg)
	}
}

// TestLitePool_Get_InterfaceTyped_UntypedNil verifies that Get on a Pool[any] does not
// panic when sync.Pool returns an untyped nil (raw == nil). The forced assertion path
// used to execute before the nil guard, so this test exercises the new ordering.
func TestLitePool_Get_InterfaceTyped_UntypedNil(t *testing.T) {
	t.Parallel()

	// Pool[any] with a New that returns untyped nil - simulates a stateful factory that
	// has exhausted its resource. sync.Pool can also return nil when New is not set and
	// the pool is empty, but setting New here makes the behaviour deterministic.
	p := &Pool[any]{
		pool: sync.Pool{
			New: func() any {
				return nil // untyped nil; raw.(T) would panic before the nil check
			},
		},
	}

	// Must return an error, never panic.
	got, err := p.Get()
	if err == nil {
		t.Fatal("expected error for untyped nil from interface-typed pool, got nil error")
	}
	if got != nil {
		t.Errorf("expected nil zero value on error, got: %v", got)
	}
}
