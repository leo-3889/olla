package pool

// Pool is a strongly typed wrapper around sync.Pool with optional Reset() support.
// It eliminates the need for unsafe type assertions (interface{} casts) and plays
// nicely with golangci-lint. Objects returned from Get() are guaranteed to be the
// correct type. If the pooled type implements the Resettable interface, it will be
// automatically zeroed before being returned to the pool via Put().
//
// Designed for internal use where the constructor guarantees type safety, so the
// type assertion in Get() is safe and explicitly silenced.
//
// Example:
//   type RequestContext struct { ... }
//   func (r *RequestContext) Reset() { ... }
//
//   pool, err := NewLitePool(func() *RequestContext {
//     return &RequestContext{}
//   })
//   if err != nil {
//     // handle error
//   }
//
//   ctx := pool.Get()
//   ...
//   pool.Put(ctx)
//
// Note: This is intentionally minimal and inlined for performance-sensitive paths.
// If Go ever adds generics to sync.Pool (e.g. Go 1.23+), this becomes obsolete.

import (
	"errors"
	"reflect"
	"sync"
)

type Resettable interface {
	Reset()
}

type Pool[T any] struct {
	pool sync.Pool
	new  func() T
}

func NewLitePool[T any](newFn func() T) (*Pool[T], error) {
	if newFn == nil {
		return nil, errors.New("litepool: constructor must not be nil")
	}
	// Validate early that the result is non-nil
	test := newFn()
	if isNilValue(test) {
		return nil, errors.New("litepool: constructor returned nil")
	}

	return &Pool[T]{
		pool: sync.Pool{
			New: func() any {
				return newFn()
			},
		},
		new: newFn,
	}, nil
}

func (p *Pool[T]) Get() T {
	//nolint:forcetypeassert // safe due to validated New
	v := p.pool.Get().(T)
	// A nil return here means the factory violated its contract after construction.
	// Panic loudly at the pool boundary rather than handing a nil pointer to callers
	// where the root cause would be impossible to attribute. This is a programmer
	// error (unrecoverable contract violation), not a runtime condition.
	if isNilValue(v) {
		panic("litepool: factory returned nil")
	}
	return v
}

// isNilValue reports whether v is nil, handling both untyped nil (interface{} == nil)
// and typed nils (e.g. (*T)(nil) stored in an interface). The reflect path is only
// reached for nilable kinds; value types like structs always return false.
func isNilValue[T any](v T) bool {
	if any(v) == nil {
		return true
	}
	rv := reflect.ValueOf(any(v))
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return rv.IsNil()
	default:
		return false
	}
}

func (p *Pool[T]) Put(v T) {
	if r, ok := any(v).(Resettable); ok {
		r.Reset()
	}
	p.pool.Put(v)
}
