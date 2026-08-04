package match

import (
	"context"
	"errors"
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

func matchForTest(t *testing.T, m Matcher, job model.Job) Result {
	t.Helper()
	result, err := m.Match(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

var entryEngineerSpec = Spec{
	Name: "all",
	Of: []Spec{
		{Name: "experience", Params: params.Map{"years": "1"}},
		{Name: "keywords", Params: params.Map{"field": "title", "include": "engineer, developer", "exclude": "senior, staff, principal, lead, manager"}},
	},
}

func TestEmploymentMatcher(t *testing.T) {
	m := build(t, Spec{Name: "employment", Params: params.Map{"types": "full-time"}})

	for label, want := range map[string]bool{
		"Full-time":          true,
		"FullTime":           true,
		"Full Time":          true,
		"fulltime_permanent": true,
		"Part-time":          false,
		"Contract":           false,
		"Intern":             false,
		"":                   true, // unknown passes by default
	} {
		got := matchForTest(t, m, model.Job{Title: "Engineer", EmploymentType: label})
		if got.Matched != want {
			t.Errorf("employment %q: Match() = %v, want %v (reason: %s)", label, got.Matched, want, got.Reason)
		}
	}

	strict := build(t, Spec{Name: "employment", Params: params.Map{"types": "full-time", "match_when_unknown": "false"}})
	if matchForTest(t, strict, model.Job{}).Matched {
		t.Error("unknown employment type should fail when match_when_unknown=false")
	}
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
			got := matchForTest(t, m, tt.job)
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
	if !matchForTest(t, m, model.Job{Title: "Python Developer"}).Matched {
		t.Error("any: second child matching should match")
	}
	if matchForTest(t, m, model.Job{Title: "Java Developer"}).Matched {
		t.Error("any: no child matching should not match")
	}

	notSpec := Spec{Name: "not", Of: []Spec{
		{Name: "keywords", Params: params.Map{"field": "location", "include": "US only"}},
	}}
	n := build(t, notSpec)
	if matchForTest(t, n, model.Job{Location: "Remote, US Only"}).Matched {
		t.Error("not: child match should invert to false")
	}
	if !matchForTest(t, n, model.Job{Location: "Remote, Worldwide"}).Matched {
		t.Error("not: child non-match should invert to true")
	}
}

func TestKeywordsWholeWordMatching(t *testing.T) {
	m := build(t, Spec{Name: "keywords", Params: params.Map{"exclude": "lead"}})
	if !matchForTest(t, m, model.Job{Title: "Engineer, Leadership Program"}).Matched {
		t.Error(`excluding "lead" must not block "Leadership"`)
	}
	if matchForTest(t, m, model.Job{Title: "Tech Lead"}).Matched {
		t.Error(`excluding "lead" must block "Tech Lead"`)
	}
}

func TestBuildErrors(t *testing.T) {
	cases := []Spec{
		{Name: "no-such-matcher"},
		{Name: "all"}, // combinator without children
		{Name: "not", Of: []Spec{{Name: "all"}, {Name: "all"}}}, // not with two children
		{Name: "experience", Of: []Spec{{Name: "experience"}}},  // leaf with children
		{Name: "keywords"}, // keywords without terms
		{Name: "recency"},  // recency without max_days
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
	if matchForTest(t, m, old).Matched {
		t.Error("years-old posting should not match max_days 30")
	}
	if !matchForTest(t, m, model.Job{}).Matched {
		t.Error("unknown posting date should match by default")
	}
	r := matchForTest(t, m, old)
	if !strings.Contains(r.Reason, "older than 30 days") {
		t.Errorf("reason should mention the limit: %q", r.Reason)
	}
}

type matcherFunc struct {
	name string
	fn   func(context.Context) (Result, error)
}

func (m matcherFunc) Name() string { return m.name }
func (m matcherFunc) Match(ctx context.Context, _ model.Job) (Result, error) {
	return m.fn(ctx)
}

func fixedMatcher(name string, matched bool, err error) Matcher {
	return matcherFunc{name: name, fn: func(context.Context) (Result, error) {
		return Result{Matched: matched, Reason: name}, err
	}}
}

func TestCombinatorErrorSemantics(t *testing.T) {
	boom := errors.New("provider unavailable")

	result, err := (&all{children: []Matcher{
		fixedMatcher("unknown", false, boom),
		fixedMatcher("veto", false, nil),
	}}).Match(context.Background(), model.Job{})
	if err != nil || result.Matched {
		t.Fatalf("all(error, false) = (%+v, %v), want false, nil", result, err)
	}
	if _, err := (&all{children: []Matcher{
		fixedMatcher("unknown", false, boom),
		fixedMatcher("allow", true, nil),
	}}).Match(context.Background(), model.Job{}); !errors.Is(err, boom) {
		t.Fatalf("all(error, true) error = %v, want %v", err, boom)
	}

	result, err = (&any_{children: []Matcher{
		fixedMatcher("unknown", false, boom),
		fixedMatcher("allow", true, nil),
	}}).Match(context.Background(), model.Job{})
	if err != nil || !result.Matched {
		t.Fatalf("any(error, true) = (%+v, %v), want true, nil", result, err)
	}
	if _, err := (&any_{children: []Matcher{
		fixedMatcher("unknown", false, boom),
		fixedMatcher("veto", false, nil),
	}}).Match(context.Background(), model.Job{}); !errors.Is(err, boom) {
		t.Fatalf("any(error, false) error = %v, want %v", err, boom)
	}

	if _, err := (&not{child: fixedMatcher("unknown", false, boom)}).Match(context.Background(), model.Job{}); !errors.Is(err, boom) {
		t.Fatalf("not(error) error = %v, want %v", err, boom)
	}
}

func TestCombinatorShortCircuitAndCancellation(t *testing.T) {
	called := 0
	countedError := matcherFunc{name: "unexpected", fn: func(context.Context) (Result, error) {
		called++
		return Result{}, errors.New("should not run")
	}}
	if result, err := (&all{children: []Matcher{
		fixedMatcher("veto", false, nil), countedError,
	}}).Match(context.Background(), model.Job{}); err != nil || result.Matched || called != 0 {
		t.Fatalf("all short circuit = (%+v, %v), later calls=%d", result, err, called)
	}
	if result, err := (&any_{children: []Matcher{
		fixedMatcher("allow", true, nil), countedError,
	}}).Match(context.Background(), model.Job{}); err != nil || !result.Matched || called != 0 {
		t.Fatalf("any short circuit = (%+v, %v), later calls=%d", result, err, called)
	}

	ctx, cancel := context.WithCancel(context.Background())
	canceling := matcherFunc{name: "cancel", fn: func(context.Context) (Result, error) {
		cancel()
		return Result{}, context.Canceled
	}}
	if _, err := (&any_{children: []Matcher{canceling, countedError}}).Match(ctx, model.Job{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled any error = %v", err)
	}
	if called != 0 {
		t.Fatalf("combinator called %d children after cancellation", called)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancelingNonMatch := matcherFunc{name: "cancel", fn: func(context.Context) (Result, error) {
		cancel()
		return Result{Matched: false, Reason: "not decisive for any"}, nil
	}}
	if _, err := (&any_{children: []Matcher{cancelingNonMatch, countedError}}).Match(ctx, model.Job{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled any after a non-decisive result = %v", err)
	}
	if called != 0 {
		t.Fatalf("combinator called %d children after a successful child canceled context", called)
	}
}
