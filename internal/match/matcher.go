// Package match decides which jobs are worth notifying about. The matching
// algorithm is expected to evolve — implement the Matcher interface in a new
// file, Register it in init(), and select it in config under `matcher:`.
//
// Matchers compose: the "all", "any", and "not" combinators take child
// matchers under `of:` in the config, so rules like "experience <= 1 AND
// title looks like engineering AND NOT US-only" are pure configuration.
package match

import (
	"fmt"
	"sort"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

// Result is a matcher's verdict on one job.
type Result struct {
	Matched bool
	// Reason is a human-readable explanation shown in notifications,
	// e.g. `asks for ~1 year experience: "1+ years working with Go"`.
	Reason string
}

// Matcher decides whether a job should trigger a notification.
type Matcher interface {
	Name() string
	Match(job model.Job) Result
}

// Spec is a matcher configuration tree, mirroring the `matcher:` block in
// the config file. Leaf matchers use Name+Params; combinators additionally
// nest children under Of.
type Spec struct {
	Name   string
	Params params.Map
	Of     []Spec
}

// Factory builds a Matcher from its params and already-built children.
// Leaf matchers should reject children (see RequireNoChildren).
type Factory func(p params.Map, children []Matcher) (Matcher, error)

var registry = map[string]Factory{}

// Register makes a matcher available under the given name.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("match: duplicate registration of " + name)
	}
	registry[name] = f
}

// Build constructs the matcher tree described by spec.
func Build(spec Spec) (Matcher, error) {
	f, ok := registry[spec.Name]
	if !ok {
		return nil, fmt.Errorf("unknown matcher %q (available: %v)", spec.Name, Names())
	}
	children := make([]Matcher, 0, len(spec.Of))
	for i, cs := range spec.Of {
		child, err := Build(cs)
		if err != nil {
			return nil, fmt.Errorf("%s: child %d: %w", spec.Name, i+1, err)
		}
		children = append(children, child)
	}
	m, err := f(spec.Params, children)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Name, err)
	}
	return m, nil
}

// RequireNoChildren is a helper for leaf matcher factories.
func RequireNoChildren(name string, children []Matcher) error {
	if len(children) > 0 {
		return fmt.Errorf("%q does not take children under \"of:\"", name)
	}
	return nil
}

// Names lists registered matchers, sorted, for error messages.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
