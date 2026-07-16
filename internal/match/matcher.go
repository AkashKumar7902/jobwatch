// Package match decides which jobs are worth notifying about. The matching
// algorithm is expected to evolve — implement the Matcher interface in a new
// file, Register it in init(), and select it in config under `matcher:`.
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

// Factory builds a Matcher from its config params.
type Factory func(p params.Map) (Matcher, error)

var registry = map[string]Factory{}

// Register makes a matcher available under the given name.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("match: duplicate registration of " + name)
	}
	registry[name] = f
}

// New builds the matcher registered under name.
func New(name string, p params.Map) (Matcher, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown matcher %q (available: %v)", name, Names())
	}
	return f(p)
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
