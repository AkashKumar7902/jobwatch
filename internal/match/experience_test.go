package match

import (
	"strings"
	"testing"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

// The matcher's rule: the job matches when YOUR years of experience fall
// inside any experience range the posting states.
func TestExperienceMatcher(t *testing.T) {
	oneYear := &Experience{Years: 1}

	tests := []struct {
		name    string
		title   string
		desc    string
		matched bool
	}{
		// 1 year inside the stated range/floor.
		{"one plus years", "Backend Engineer", "You have 1+ years of experience with Go.", true},
		{"zero plus years", "Support Engineer", "0+ years experience welcome.", true},
		{"range containing 1", "Junior Dev", "We look for 0-2 years of professional experience.", true},
		{"range starting at one, spelled 'to'", "QA Analyst", "1 to 3 years experience in testing.", true},
		{"months floor below 1yr", "Intern", "At least 6 months of experience with Python.", true},
		{"months range containing 1yr", "Junior Analyst", "6-18 months of experience with SQL required.", true},
		{"word number one", "Analyst", "one year of experience working with SQL", true},
		{"word range", "Junior Analyst", "One to three years of experience working with data.", true},
		{"up to phrasing", "Associate", "Up to 2 years of professional experience.", true},
		{"entry level wording", "Engineer I", "This is an entry-level position.", true},
		{"fresher wording", "Trainee", "Freshers are encouraged to apply.", true},
		{"no experience required", "Junior Support", "No prior experience required, we train you.", true},
		{"any of several mentions fits", "Platform Engineer", "5+ years backend experience. 1+ years working with Kubernetes.", true},
		{"multibyte text near figure", "Engineer", "私たちのチームで働きませんか — 1+ years of experience with Go — リモート勤務可能です", true},

		// 1 year outside the stated range/floor.
		{"three plus years", "Engineer", "3+ years of experience building APIs.", false},
		{"senior range", "Senior Engineer", "We require 5-7 years of relevant industry experience.", false},
		{"two years bare floor", "Developer", "Minimum 2 years experience with React.", false},
		{"word number five", "Architect", "five years of professional experience", false},
		{"decimal floor above 1", "Engineer", "1.5+ years of experience with Rust required.", false},
		{"range plus is a floor", "Engineer", "2-3+ years of experience with Java.", false},

		// No requirement listed, or decoys only.
		{"nothing listed", "Engineer", "You will build things with us. Great benefits.", false},
		{"years ago is not a requirement", "Engineer", "Founded 10 years ago, we ship software. Our team works remotely.", false},
		{"years of age is not a requirement", "Moderator", "Applicants must be 18 years of age. Work with our moderation team. 4+ years of trust & safety experience required.", false},
		{"onboarding months ignored", "Staff Engineer", "In your first 3 months you will ship features. Requires 8+ years of experience.", false},
		{"onboarding years ignored", "Engineer", "In your first 2 years you will grow with us. Requires 6+ years of software experience.", false},
		{"negated entry level", "Senior Engineer", "This is not an entry-level position. 8+ years of experience required.", false},
		{"'piano experience required' is not 'no experience required'", "Music Teacher", "Piano experience required for this role.", false},

		// Regressions from adversarial testing.
		{"between phrasing", "Backend Engineer", "We require between 1 and 3 years of experience in Go.", true},
		{"up to with word number", "Analyst", "Up to two years of work experience is expected.", true},
		{"benefits months in next sentence", "Senior Backend Engineer", "Requires 8+ years of experience. We offer 12 months paid parental leave and 25 days PTO.", false},
		{"'no freshers' is a negation", "Senior Data Engineer", "No freshers, please. We need 5+ years of hands-on data engineering experience.", false},
		{"curly apostrophe negation", "Senior Analyst", "This isn’t an entry-level position. Minimum 7 years of professional experience.", false},
		{"'not for new grads' is a negation", "Senior SRE", "This role is not for new grads. 6+ years of production experience required.", false},
		{"visa duration ignored", "Distinguished Engineer", "Requires 10+ years of experience. We sponsor work visas valid for 1 year with annual renewal.", false},
		{"runway ignored even near requirement", "Senior Engineer", "Well-funded with 2 years of runway. Seeking engineers with 6+ years of experience.", false},
		{"onboarding months after figure", "Senior Platform Engineer", "Requires 8+ years of experience. You'll get 6 months of onboarding with experienced mentors.", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := model.Job{Title: tt.title, Description: tt.desc}
			got := oneYear.Match(job)
			if got.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v (reason: %s)", got.Matched, tt.matched, got.Reason)
			}
		})
	}
}

// The key difference from a "minimum <= N" rule: a bounded range REJECTS
// candidates above its ceiling.
func TestExperienceUpperBoundBinds(t *testing.T) {
	threeYears := &Experience{Years: 3}

	tests := []struct {
		desc    string
		matched bool
	}{
		{"0-1 years of experience preferred.", false}, // 3 is above the ceiling
		{"Up to 2 years of professional experience.", false},
		{"6-18 months of experience with SQL.", false},
		{"1-3 years of experience with Go.", true},
		{"1+ years of experience with Go.", true},
		{"Entry-level role, no experience required.", true}, // floor-only, anyone qualifies
		{"We require between 1 and 3 years of experience in Go.", true},
		{"Up to two years of work experience is expected.", false}, // ceiling is 2
	}
	for _, tt := range tests {
		got := threeYears.Match(model.Job{Title: "Engineer", Description: tt.desc})
		if got.Matched != tt.matched {
			t.Errorf("years=3 vs %q: Match() = %v, want %v (reason: %s)", tt.desc, got.Matched, tt.matched, got.Reason)
		}
	}
}

// Decoys aimed at candidates whose own years coincide with a decoy figure.
func TestExperienceDecoyFigures(t *testing.T) {
	cases := []struct {
		years float64
		desc  string
	}{
		{18, "Must be 18 years or older and authorized to work in the US. 4-6 years of experience preferred."},
		{10, "For 10 years we have worked with top consumer brands. We need someone with 3-5 years of experience."},
		{2, "Well-funded with 2 years of runway. Seeking engineers with 6+ years of experience."},
		{2, "Our team ingested 2 years of network telemetry into the warehouse; the pipeline never pages anyone. Qualifications: 5+ years of professional experience running production infrastructure."},
	}
	for _, tt := range cases {
		m := &Experience{Years: tt.years}
		got := m.Match(model.Job{Title: "Engineer", Description: tt.desc})
		if got.Matched {
			t.Errorf("years=%v vs %q: decoy figure treated as requirement (reason: %s)", tt.years, tt.desc, got.Reason)
		}
	}
}

func TestExperienceUnlistedFlag(t *testing.T) {
	job := model.Job{Title: "Engineer", Description: "Come build with us."}

	strict := &Experience{Years: 1, NotifyWhenUnlisted: false}
	if strict.Match(job).Matched {
		t.Error("unlisted experience should not match when NotifyWhenUnlisted=false")
	}
	lax := &Experience{Years: 1, NotifyWhenUnlisted: true}
	if !lax.Match(job).Matched {
		t.Error("unlisted experience should match when NotifyWhenUnlisted=true")
	}
}

func TestExperienceRejectsOldParam(t *testing.T) {
	_, err := Build(Spec{Name: "experience", Params: params.Map{"max_years": "1"}})
	if err == nil {
		t.Error("the renamed max_years param should produce a helpful error")
	}
}

func TestReasonIncludesSnippetAndRange(t *testing.T) {
	m := &Experience{Years: 1}
	res := m.Match(model.Job{Title: "Junior Engineer", Description: "We want someone with 1-3 years of experience shipping Go services."})
	if !res.Matched {
		t.Fatalf("expected match, got %q", res.Reason)
	}
	for _, want := range []string{"1-3 years", "1 year"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q should mention %q", res.Reason, want)
		}
	}
}
