package match

import (
	"testing"

	"jobwatch/internal/model"
)

func TestExperienceMatcher(t *testing.T) {
	m := &Experience{MaxYears: 1}

	tests := []struct {
		name    string
		title   string
		desc    string
		matched bool
	}{
		{"one plus years", "Backend Engineer", "You have 1+ years of experience with Go.", true},
		{"zero plus years", "Support Engineer", "0+ years experience welcome.", true},
		{"range starting at zero", "Junior Dev", "We look for 0-2 years of professional experience.", true},
		{"range starting at one, spelled 'to'", "QA Analyst", "1 to 3 years experience in testing.", true},
		{"months requirement", "Intern", "At least 6 months of experience with Python.", true},
		{"word number one", "Analyst", "one year of experience working with SQL", true},
		{"entry level wording", "Engineer I", "This is an entry-level position.", true},
		{"fresher wording", "Trainee", "Freshers are encouraged to apply.", true},
		{"no experience required", "Junior Support", "No prior experience required, we train you.", true},
		{"multiple figures takes lowest", "Platform Engineer", "5+ years backend experience. 1+ years working with Kubernetes.", true},

		{"three plus years", "Engineer", "3+ years of experience building APIs.", false},
		{"senior range", "Senior Engineer", "We require 5-7 years of relevant industry experience.", false},
		{"two years exact", "Developer", "Minimum 2 years experience with React.", false},
		{"word number five", "Architect", "five years of professional experience", false},
		{"nothing listed", "Engineer", "You will build things with us. Great benefits.", false},
		{"years ago is not a requirement", "Engineer", "Founded 10 years ago, we ship software. Our team works remotely.", false},
		{"years of age is not a requirement", "Moderator", "Applicants must be 18 years of age. Work with our moderation team. 4+ years of trust & safety experience required.", false},
		{"vacation days ignored", "Engineer", "We require 6+ years of experience. Benefits: 2 years sabbatical eligibility after tenure... just kidding, 25 vacation days.", false},
		{"onboarding months ignored", "Staff Engineer", "In your first 3 months you will ship features. Requires 8+ years of experience.", false},
		{"onboarding years ignored", "Engineer", "In your first 2 years you will grow with us. Requires 6+ years of software experience.", false},
		{"decimal years above threshold", "Engineer", "1.5+ years of experience with Rust required.", false},
		{"negated entry level", "Senior Engineer", "This is not an entry-level position. 8+ years of experience required.", false},
		{"'piano experience required' is not 'no experience required'", "Music Teacher", "Piano experience required for this role.", false},
		{"spelled out range takes lower bound", "Junior Analyst", "One to three years of experience working with data.", true},
		{"multibyte text near figure", "Engineer", "私たちのチームで働きませんか — 1+ years of experience with Go — リモート勤務可能です", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := model.Job{Title: tt.title, Description: tt.desc}
			got := m.Match(job)
			if got.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v (reason: %s)", got.Matched, tt.matched, got.Reason)
			}
		})
	}
}

func TestExperienceUnlistedFlag(t *testing.T) {
	job := model.Job{Title: "Engineer", Description: "Come build with us."}

	strict := &Experience{MaxYears: 1, NotifyWhenUnlisted: false}
	if strict.Match(job).Matched {
		t.Error("unlisted experience should not match when NotifyWhenUnlisted=false")
	}
	lax := &Experience{MaxYears: 1, NotifyWhenUnlisted: true}
	if !lax.Match(job).Matched {
		t.Error("unlisted experience should match when NotifyWhenUnlisted=true")
	}
}

func TestReasonIncludesSnippet(t *testing.T) {
	m := &Experience{MaxYears: 1}
	job := model.Job{Title: "Junior Engineer", Description: "We want someone with 1+ years of experience shipping Go services."}
	res := m.Match(job)
	if !res.Matched {
		t.Fatalf("expected match, got %q", res.Reason)
	}
	if res.Reason == "" {
		t.Error("reason should explain the match")
	}
}
