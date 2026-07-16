// Package source fetches job postings from applicant tracking systems
// (ATS). Each ATS gets one file implementing the Source interface and
// registering a Factory in init(); the config file then selects sources by
// name. To support a new ATS, add one file — nothing else changes.
package source

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

// Source fetches the current openings of one company from one ATS.
type Source interface {
	// Company is the display name used in logs and notifications.
	Company() string
	// Fetch returns all currently open jobs, normalized.
	Fetch(ctx context.Context) ([]model.Job, error)
}

// Factory builds a Source for one company from its config entry.
type Factory func(company string, p params.Map, client *http.Client) (Source, error)

var registry = map[string]Factory{}

// Register makes a source type available under the given name.
// Call it from init() in the file implementing the source.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("source: duplicate registration of " + name)
	}
	registry[name] = f
}

// New builds the source type registered under name.
func New(name, company string, p params.Map, client *http.Client) (Source, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown source type %q (available: %v)", name, Names())
	}
	return f(company, p, client)
}

// Names lists registered source types, sorted, for error messages.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
