// Copyright JAMF Software, LLC

package iterx

import (
	"iter"
	_ "unsafe"
)

func Collect[T any](seq iter.Seq[T]) (res []T) {
	seq(func(t T) bool {
		res = append(res, t)
		return true
	})
	return
}

func Consume[T any](seq iter.Seq[T], fn func(T)) {
	seq(func(t T) bool {
		fn(t)
		return true
	})
}

func From[T any](in ...T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, item := range in {
			if !yield(item) {
				return
			}
		}
	}
}

func First[T any](seq iter.Seq[T]) T {
	var res T
	if seq == nil {
		return *new(T)
	}
	seq(func(t T) bool {
		res = t
		return false
	})
	return res
}

func Map[S, R any](seq iter.Seq[S], fn func(S) R) iter.Seq[R] {
	return func(yield func(R) bool) {
		seq(func(v S) bool {
			return yield(fn(v))
		})
	}
}

func Contains[T comparable](seq iter.Seq[T], item T) bool {
	found := false
	seq(func(t T) bool {
		if t == item {
			found = true
			return false
		}
		return true
	})
	return found
}
