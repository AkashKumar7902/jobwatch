// Package notify delivers matched jobs to the user. Notifiers follow the
// same registry pattern as sources: one file per channel, Register in
// init(), selected by name in config under `notifiers:`.
package notify

import (
	"context"
	"fmt"
	"sort"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

// Match pairs a job with the matcher's explanation for it.
type Match struct {
	Job    model.Job
	Reason string
}

// Notifier delivers a batch of matches through one channel.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, matches []Match) error
}

// Factory builds a Notifier from its config params.
type Factory func(p params.Map) (Notifier, error)

var registry = map[string]Factory{}

// Register makes a notifier available under the given name.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("notify: duplicate registration of " + name)
	}
	registry[name] = f
}

// New builds the notifier registered under name.
func New(name string, p params.Map) (Notifier, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown notifier %q (available: %v)", name, Names())
	}
	return f(p)
}

// Names lists registered notifiers, sorted, for error messages.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
