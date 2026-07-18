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
	"strings"

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

// identifiedSource adds a stable board identity to a Source. The identity is
// derived from connection params only (never the display name or operational
// limits), so renaming a company or changing max_postings does not make an
// existing board look new.
type identifiedSource struct {
	Source
	identity    string
	statePrefix string
}

func (s *identifiedSource) Identity() string    { return s.identity }
func (s *identifiedSource) StatePrefix() string { return s.statePrefix }

// Identity returns the canonical ATS board identity for s. Sources created by
// New always have one; the Company fallback keeps hand-written test sources
// and third-party implementations compatible.
func Identity(s Source) string {
	if identified, ok := s.(interface{ Identity() string }); ok {
		return identified.Identity()
	}
	return "custom/" + s.Company()
}

// StatePrefix returns the prefix used by this source's stable model.Job IDs.
// It lets state migrations recognize an already-used board even when none of
// its currently open postings overlap the latest fetch.
func StatePrefix(s Source) string {
	if identified, ok := s.(interface{ StatePrefix() string }); ok {
		return identified.StatePrefix()
	}
	return ""
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
	s, err := f(company, p, client)
	if err != nil {
		return nil, err
	}
	return &identifiedSource{
		Source: s, identity: identityFor(name, p), statePrefix: statePrefixFor(name, p),
	}, nil
}

func identityFor(name string, p params.Map) string {
	switch name {
	case "greenhouse":
		return fmt.Sprintf("greenhouse/%s/%s", p.GetDefault("region", "us"), p.Get("board_token"))
	case "lever":
		return fmt.Sprintf("lever/%s/%s", p.GetDefault("region", "us"), p.Get("site"))
	case "ashby":
		return "ashby/" + p.Get("board_name")
	case "workable":
		return "workable/" + p.Get("account")
	case "recruitee":
		return "recruitee/" + p.Get("company_slug")
	case "smartrecruiters":
		return "smartrecruiters/" + p.Get("company_id")
	case "bamboohr":
		return "bamboohr/" + p.Get("company_slug")
	case "workday":
		return fmt.Sprintf("workday/%s/%s/%s", p.Get("host"), p.Get("tenant"), p.Get("site"))
	}

	// Keep future externally registered sources deterministic too.
	keys := make([]string, 0, len(p))
	for key := range p {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, key+"="+p[key])
	}
	return name + "/" + strings.Join(parts, ",")
}

func statePrefixFor(name string, p params.Map) string {
	switch name {
	case "greenhouse":
		return "greenhouse/" + p.Get("board_token") + "/"
	case "lever":
		return "lever/" + p.Get("site") + "/"
	case "ashby":
		return "ashby/" + p.Get("board_name") + "/"
	case "workable":
		return "workable/" + p.Get("account") + "/"
	case "recruitee":
		return "recruitee/" + p.Get("company_slug") + "/"
	case "smartrecruiters":
		return "smartrecruiters/" + p.Get("company_id") + "/"
	case "bamboohr":
		return "bamboohr/" + p.Get("company_slug") + "/"
	case "workday":
		base := fmt.Sprintf("https://%s/wday/cxs/%s/%s", p.Get("host"), p.Get("tenant"), p.Get("site"))
		return "workday/" + base + "/"
	}
	return ""
}

// Detailer is implemented by sources whose list endpoint lacks the full
// posting (description, employment type). The runner calls Detail only for
// jobs it actually evaluates — new or retried ones — so whole boards can be
// listed completely without paying one request per posting per run.
type Detailer interface {
	Detail(ctx context.Context, job *model.Job) error
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
