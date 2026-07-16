package source

import (
	"net/http"
	"testing"

	"jobwatch/internal/params"
)

func TestIdentityUsesBoardParamsNotOperationalLimits(t *testing.T) {
	a, err := New("workday", "Acme", params.Map{
		"host": "acme.wd1.myworkdayjobs.com", "tenant": "acme", "site": "jobs", "max_postings": "20",
	}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("workday", "Renamed Acme", params.Map{
		"host": "acme.wd1.myworkdayjobs.com", "tenant": "acme", "site": "jobs", "max_postings": "200",
	}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if Identity(a) != Identity(b) {
		t.Fatalf("operational limit/display rename changed identity: %q != %q", Identity(a), Identity(b))
	}
	wantPrefix := "workday/https://acme.wd1.myworkdayjobs.com/wday/cxs/acme/jobs/"
	if got := StatePrefix(a); got != wantPrefix {
		t.Fatalf("StatePrefix() = %q, want %q", got, wantPrefix)
	}
}

func TestIdentityIncludesRegion(t *testing.T) {
	us, err := New("greenhouse", "Acme US", params.Map{"board_token": "acme"}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	eu, err := New("greenhouse", "Acme EU", params.Map{"board_token": "acme", "region": "eu"}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if Identity(us) == Identity(eu) {
		t.Fatalf("regional boards collided at %q", Identity(us))
	}
}

func TestMaxPostingsMustBePositive(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    params.Map
	}{
		{"smartrecruiters", params.Map{"company_id": "Acme", "max_postings": "-1"}},
		{"workday", params.Map{"host": "acme.wd1.myworkdayjobs.com", "tenant": "acme", "site": "jobs", "max_postings": "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.name, "Acme", tc.p, &http.Client{}); err == nil {
				t.Fatal("expected invalid max_postings to fail")
			}
		})
	}
}
