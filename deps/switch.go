package deps

import (
	"context"
	"sync/atomic"
)

// Switch serves a primary backend while its dependency is up and a local
// fallback otherwise. Fallback state is never merged back: every down→up
// transition replaces the fallback with a fresh one.
type Switch[T any] struct {
	dep         *Dependency
	primary     T
	hasPrimary  bool
	newFallback func() T
	fallback    atomic.Pointer[T]
}

// NewSwitch builds a Switch. A nil dep means no primary is configured and the
// fallback is always used.
func NewSwitch[T any](dep *Dependency, primary T, newFallback func() T) *Switch[T] {
	s := &Switch[T]{dep: dep, primary: primary, hasPrimary: dep != nil, newFallback: newFallback}
	fb := newFallback()
	s.fallback.Store(&fb)
	if dep != nil {
		dep.OnUp(func(context.Context) {
			fb := s.newFallback()
			s.fallback.Store(&fb)
		})
	}
	return s
}

// Get returns the backend to use now and whether it is the primary.
func (s *Switch[T]) Get() (T, bool) {
	if s.hasPrimary && s.dep.Up() {
		return s.primary, true
	}
	return *s.fallback.Load(), false
}

// Dependency returns the dependency that gates the primary (nil if none).
func (s *Switch[T]) Dependency() *Dependency { return s.dep }

// Call runs fn on the current backend. When the primary fails with an
// unavailability error the dependency is marked down and fn is retried once on
// the fallback, so the caller never sees the outage.
func Call[T, R any](s *Switch[T], fn func(T) (R, error)) (R, error) {
	b, primary := s.Get()
	r, err := fn(b)
	if primary && s.dep.Report(err) {
		return fn(*s.fallback.Load())
	}
	return r, err
}

// Do is Call for functions without a result.
func Do[T any](s *Switch[T], fn func(T) error) error {
	_, err := Call(s, func(b T) (struct{}, error) { return struct{}{}, fn(b) })
	return err
}
