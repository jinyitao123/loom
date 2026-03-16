package loom

import (
	"context"
	"fmt"
)

// Router determines the next step based on current state.
// Returns the name of the next step, or "" to halt.
type Router func(ctx context.Context, state State) (string, error)

// Always returns the same next step.
func Always(next string) Router {
	return func(_ context.Context, _ State) (string, error) { return next, nil }
}

// End always halts the graph.
func End() Router {
	return func(_ context.Context, _ State) (string, error) { return "", nil }
}

// Branch routes based on a state key's string value.
func Branch(key string, routes map[string]string, fallback string) Router {
	return func(_ context.Context, s State) (string, error) {
		var val string
		switch v := s[key].(type) {
		case string:
			val = v
		case nil:
			val = ""
		default:
			val = fmt.Sprintf("%v", v)
		}
		if next, ok := routes[val]; ok {
			return next, nil
		}
		return fallback, nil
	}
}

// BranchFunc routes based on a user-supplied key extractor.
func BranchFunc(extract func(State) string, routes map[string]string, fallback string) Router {
	return func(_ context.Context, s State) (string, error) {
		val := extract(s)
		if next, ok := routes[val]; ok {
			return next, nil
		}
		return fallback, nil
	}
}

// Condition routes based on a predicate.
func Condition(pred func(State) bool, ifTrue, ifFalse string) Router {
	return func(_ context.Context, s State) (string, error) {
		if pred(s) {
			return ifTrue, nil
		}
		return ifFalse, nil
	}
}
