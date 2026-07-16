package match

import (
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func build(t *testing.T, spec Spec) Matcher {
	t.Helper()
	m, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var entryEngineerSpec = Spec{
	Name: "all",
	Of: []Spec{
		{Name: "experience", Params: params.Map{"max_years": "1"}},
		{Name: "keywords", Params: params.Map{"field": "title", "include": "engineer, developer", "exclude": "senior, staff, principal, lead, manager"}},
	},
}

func TestAllCombinator(t *testing.T) {
	m := build(t, entryEngineerSpec)

	tests := []struct {
		name    string
		job     model.Job
		matched bool
	}{
		{"entry engineer", model.Job{Title: "Software Engineer", Description: "0-1 years of experience."}, true},
		{"entry but wrong role", model.Job{Title: "Sales Development Representative", Description: "1+ years of sales experience."}, false},
		{"engineer but senior title", model.Job{Title: "Senior Engineer", Description: "1+ years with Kubernetes experience."}, false},
		{"engineer but too senior", model.Job{Title: "Software Engineer", Description: "7+ years of experience."}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Match(tt.job)
			if got.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v (reason: %s)", got.Matched, tt.matched, got.Reason)
			}
			if got.Reason == "" {
				t.Error("combinators must always explain their verdict")
			}
		})
	}
}

func TestAnyAndNotCombinators(t *testing.T) {
	anySpec := Spec{Name: "any", Of: []Spec{
		{Name: "keywords", Params: params.Map{"include": "golang"}},
		{Name: "keywords", Params: params.Map{"include": "python"}},
	}}
	m := build(t, anySpec)
	if !m.Match(model.Job{Title: "Python Developer"}).Matched {
		t.Error("any: second child matching should match")
	}
	if m.Match(model.Job{Title: "Java Developer"}).Matched {
		t.Error("any: no child matching should not match")
	}

	notSpec := Spec{Name: "not", Of: []Spec{
		{Name: "keywords", Params: params.Map{"field": "location", "include": "US only"}},
	}}
	n := build(t, notSpec)
	if n.Match(model.Job{Location: "Remote, US Only"}).Matched {
		t.Error("not: child match should invert to false")
	}
	if !n.Match(model.Job{Location: "Remote, Worldwide"}).Matched {
		t.Error("not: child non-match should invert to true")
	}
}

func TestKeywordsWholeWordMatching(t *testing.T) {
	m := build(t, Spec{Name: "keywords", Params: params.Map{"exclude": "lead"}})
	if !m.Match(model.Job{Title: "Engineer, Leadership Program"}).Matched {
		t.Error(`excluding "lead" must not block "Leadership"`)
	}
	if m.Match(model.Job{Title: "Tech Lead"}).Matched {
		t.Error(`excluding "lead" must block "Tech Lead"`)
	}
}

func TestBuildErrors(t *testing.T) {
	cases := []Spec{
		{Name: "no-such-matcher"},
		{Name: "all"},                                             // combinator without children
		{Name: "not", Of: []Spec{{Name: "all"}, {Name: "all"}}},   // not with two children
		{Name: "experience", Of: []Spec{{Name: "experience"}}},    // leaf with children
		{Name: "keywords"},                                        // keywords without terms
		{Name: "recency"},                                         // recency without max_days
	}
	for _, spec := range cases {
		if _, err := Build(spec); err == nil {
			t.Errorf("Build(%+v) should fail", spec)
		}
	}
}

func TestRecency(t *testing.T) {
	m := build(t, Spec{Name: "recency", Params: params.Map{"max_days": "30"}})
	old := model.Job{PostedAt: time.Now().AddDate(-1, 0, 0)}
	if m.Match(old).Matched {
		t.Error("years-old posting should not match max_days 30")
	}
	if !m.Match(model.Job{}).Matched {
		t.Error("unknown posting date should match by default")
	}
	r := m.Match(old)
	if !strings.Contains(r.Reason, "older than 30 days") {
		t.Errorf("reason should mention the limit: %q", r.Reason)
	}
}
