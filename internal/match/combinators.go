package match

// Boolean combinators. They let the config express compound rules without
// any new code:
//
//	matcher:
//	  name: all
//	  of:
//	    - name: experience
//	      params: {max_years: 1}
//	    - name: keywords
//	      params: {field: title, exclude: "senior, staff, principal"}

import (
	"errors"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("all", func(_ params.Map, children []Matcher) (Matcher, error) {
		if len(children) == 0 {
			return nil, errors.New(`needs at least one child matcher under "of:"`)
		}
		return &all{children}, nil
	})
	Register("any", func(_ params.Map, children []Matcher) (Matcher, error) {
		if len(children) == 0 {
			return nil, errors.New(`needs at least one child matcher under "of:"`)
		}
		return &any_{children}, nil
	})
	Register("not", func(_ params.Map, children []Matcher) (Matcher, error) {
		if len(children) != 1 {
			return nil, errors.New(`needs exactly one child matcher under "of:"`)
		}
		return &not{children[0]}, nil
	})
}

// all matches when every child matches; the first veto wins.
type all struct{ children []Matcher }

func (a *all) Name() string { return "all" }

func (a *all) Match(job model.Job) Result {
	reasons := make([]string, 0, len(a.children))
	for _, c := range a.children {
		r := c.Match(job)
		if !r.Matched {
			return Result{Matched: false, Reason: c.Name() + ": " + r.Reason}
		}
		reasons = append(reasons, r.Reason)
	}
	return Result{Matched: true, Reason: strings.Join(reasons, "; ")}
}

// any_ matches when at least one child matches ("any" clashes with the
// Go builtin type).
type any_ struct{ children []Matcher }

func (a *any_) Name() string { return "any" }

func (a *any_) Match(job model.Job) Result {
	reasons := make([]string, 0, len(a.children))
	for _, c := range a.children {
		r := c.Match(job)
		if r.Matched {
			return r
		}
		reasons = append(reasons, r.Reason)
	}
	return Result{Matched: false, Reason: "no criterion matched: " + strings.Join(reasons, "; ")}
}

// not inverts its child — useful for exclusion rules.
type not struct{ child Matcher }

func (n *not) Name() string { return "not" }

func (n *not) Match(job model.Job) Result {
	r := n.child.Match(job)
	return Result{Matched: !r.Matched, Reason: r.Reason}
}
