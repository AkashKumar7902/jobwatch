package match

// The "keywords" matcher checks a job field against include/exclude term
// lists (case-insensitive, whole-word). The most common use is keeping
// only relevant roles and dropping senior ones:
//
//	matcher:
//	  name: keywords
//	  params:
//	    field: title                              # title | description | location | any
//	    include: "engineer, developer, sre"       # empty = anything passes
//	    exclude: "senior, staff, principal, lead" # empty = nothing blocked
//
// Multi-word terms work ("engineering manager"). Terms match whole words,
// so excluding "lead" does not block "leadership".

import (
	"fmt"
	"regexp"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("keywords", func(p params.Map, children []Matcher) (Matcher, error) {
		if err := RequireNoChildren("keywords", children); err != nil {
			return nil, err
		}
		field := p.GetDefault("field", "title")
		switch field {
		case "title", "description", "location", "any":
		default:
			return nil, fmt.Errorf("unknown field %q (want title, description, location, or any)", field)
		}
		include, err := termsRegexp(p.Get("include"))
		if err != nil {
			return nil, fmt.Errorf("param \"include\": %w", err)
		}
		exclude, err := termsRegexp(p.Get("exclude"))
		if err != nil {
			return nil, fmt.Errorf("param \"exclude\": %w", err)
		}
		if include == nil && exclude == nil {
			return nil, fmt.Errorf(`needs "include" and/or "exclude" terms`)
		}
		return &keywords{field: field, include: include, exclude: exclude}, nil
	})
}

// termsRegexp compiles "a, b c, d" into a case-insensitive whole-word
// alternation; nil when the list is empty.
func termsRegexp(list string) (*regexp.Regexp, error) {
	var quoted []string
	for _, term := range strings.Split(list, ",") {
		if term = strings.TrimSpace(term); term != "" {
			quoted = append(quoted, regexp.QuoteMeta(term))
		}
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	return regexp.Compile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

type keywords struct {
	field            string
	include, exclude *regexp.Regexp
}

func (k *keywords) Name() string { return "keywords" }

func (k *keywords) Match(job model.Job) Result {
	var text string
	switch k.field {
	case "title":
		text = job.Title
	case "description":
		text = job.Description
	case "location":
		text = job.Location
	case "any":
		text = job.Title + "\n" + job.Location + "\n" + job.Description
	}

	if k.exclude != nil {
		if hit := k.exclude.FindString(text); hit != "" {
			return Result{Matched: false, Reason: fmt.Sprintf("%s contains excluded term %q", k.field, hit)}
		}
	}
	if k.include != nil {
		hit := k.include.FindString(text)
		if hit == "" {
			return Result{Matched: false, Reason: fmt.Sprintf("%s has none of the include terms", k.field)}
		}
		return Result{Matched: true, Reason: fmt.Sprintf("%s contains %q", k.field, hit)}
	}
	return Result{Matched: true, Reason: fmt.Sprintf("%s has no excluded terms", k.field)}
}
