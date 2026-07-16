package match

// The "employment" matcher keeps only jobs whose employment type — as the
// ATS reports it — is in the accepted list. Labels vary wildly between
// ATSes ("Full-time", "FullTime", "Full Time", "fulltime_permanent"), so
// both sides are normalized to bare letters before comparing.
//
// Greenhouse never exposes an employment type; match_when_unknown decides
// what happens then (default true, so those jobs aren't silently dropped).
//
//	matcher:
//	  name: employment
//	  params:
//	    types: "full-time"        # comma-separated accepted types
//	    match_when_unknown: true

import (
	"fmt"
	"strings"
	"unicode"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("employment", func(p params.Map, children []Matcher) (Matcher, error) {
		if err := RequireNoChildren("employment", children); err != nil {
			return nil, err
		}
		typesRaw, err := p.Require("types")
		if err != nil {
			return nil, err
		}
		var types []string
		for _, t := range strings.Split(typesRaw, ",") {
			if norm := normalizeType(t); norm != "" {
				types = append(types, norm)
			}
		}
		if len(types) == 0 {
			return nil, fmt.Errorf(`param "types": no usable values`)
		}
		unknown, err := p.Bool("match_when_unknown", true)
		if err != nil {
			return nil, err
		}
		return &employment{types: types, matchUnknown: unknown}, nil
	})
}

// normalizeType lowercases and drops everything but letters, so
// "Full-Time", "full time", and "FullTime" all become "fulltime".
func normalizeType(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type employment struct {
	types        []string // normalized accepted types
	matchUnknown bool
}

func (e *employment) Name() string { return "employment" }

func (e *employment) Match(job model.Job) Result {
	if job.EmploymentType == "" {
		return Result{Matched: e.matchUnknown, Reason: "employment type not stated"}
	}
	norm := normalizeType(job.EmploymentType)
	for _, t := range e.types {
		// Containment absorbs suffixes like recruitee's "fulltime_permanent".
		if strings.Contains(norm, t) {
			return Result{Matched: true, Reason: fmt.Sprintf("employment type %q", job.EmploymentType)}
		}
	}
	return Result{Matched: false, Reason: fmt.Sprintf("employment type %q not in accepted list", job.EmploymentType)}
}
