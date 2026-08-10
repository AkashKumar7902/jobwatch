package source

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"jobwatch/internal/config"
	"jobwatch/internal/params"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/identities.golden from the live catalog")

const identitiesGolden = "testdata/identities.golden"

// TestCatalogIdentitiesMatchGolden walks every company in the live catalog,
// builds its source, and pins Identity and StatePrefix byte-for-byte.
//
// This is the regression test for the refactor that introduced coordsFor:
// the two derived strings must be IDENTICAL to what the two old parallel
// switches produced for every adapter whose params did not change meaning.
// A drifting adapter does not look like a bug at runtime — it looks like a
// board that quietly re-baselines and stops emailing — so the only place it
// can be caught is here, against the real 263-company catalog rather than a
// hand-picked sample.
//
// A deliberate change (a new company, or another param declared transport)
// means regenerating: go test ./internal/source -run Golden -update
func TestCatalogIdentitiesMatchGolden(t *testing.T) {
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, c := range cfg.Companies {
		s, err := New(c.Source, c.Name, c.Params, &http.Client{})
		if err != nil {
			t.Fatalf("company %q: %v", c.Name, err)
		}
		fmt.Fprintf(&got, "%s\t%s\t%s\t%s\n", c.Name, c.Source, Identity(s), StatePrefix(s))
	}
	if *updateGolden {
		if err := os.WriteFile(identitiesGolden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote " + identitiesGolden)
		return
	}
	wantBytes, err := os.ReadFile(identitiesGolden)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimRight(string(wantBytes), "\n"), "\n")
	gotLines := strings.Split(strings.TrimRight(got.String(), "\n"), "\n")
	for i := range min(len(wantLines), len(gotLines)) {
		if gotLines[i] != wantLines[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i+1, gotLines[i], wantLines[i])
		}
	}
	if len(gotLines) != len(wantLines) {
		t.Fatalf("catalog has %d boards, golden has %d (regenerate with -update)", len(gotLines), len(wantLines))
	}
}

// TestEveryRegisteredSourceIsDeclared makes the coordinate table exhaustive.
// An adapter missing from coordsFor still works, but it falls into the
// externally-registered fallback: a "k=v,k=v" identity and an EMPTY state
// prefix, which the runner reads as "cannot prove this board is new". That is
// a silent downgrade, so the test turns it into a build-time conversation
// about what the new adapter's stable coordinates are.
func TestEveryRegisteredSourceIsDeclared(t *testing.T) {
	for _, name := range Names() {
		if _, known := coordsFor(name, params.Map{}); !known {
			t.Errorf("source %q has no coordsFor case: declare its stable, scoped and volatile params", name)
		}
	}
}

// TestCoordDeclarationsAreWellFormed checks the flag combinations that the
// two projections would silently misread.
func TestCoordDeclarationsAreWellFormed(t *testing.T) {
	// Params that satisfy every adapter's factory are not needed here: the
	// declaration's SHAPE does not depend on values.
	sample := params.Map{
		"host": "board.example.com", "domain": "example.com", "tenant": "acme",
		"site": "jobs", "board": "d6eed263-4950-420d-b9f8-5b1a441c931e",
		"company_id": "15470", "appid": "careers", "scope": "careers2", "rc": "in",
		"domain_code": "acme", "request_type": "token", "company_code": "code",
		"location": "India", "query": "Platform",
	}
	for _, name := range Names() {
		coords, known := coordsFor(name, sample)
		if !known {
			continue
		}
		for _, c := range coords {
			if c.Key == "" {
				t.Errorf("%s: coord %q has no key; the key is what makes the declaration reviewable", name, c.Value)
			}
			if c.Volatile && c.Namespace {
				t.Errorf("%s: coord %q is both volatile and in the namespace; a transport value in a job ID is exactly the bug being fixed", name, c.Key)
			}
			if c.PrefixOnly && !c.Namespace {
				t.Errorf("%s: coord %q is prefix-only but not in the namespace, so it appears nowhere at all", name, c.Key)
			}
		}
	}
}

// keyPrefixOf returns the prefix an adapter stamps onto its own job IDs. It
// exists so a test can assert the third guard — the job IDs themselves — and
// not just the two strings the source package exports.
func keyPrefixOf(t *testing.T, s Source) string {
	t.Helper()
	wrapped, ok := s.(*identifiedSource)
	if !ok {
		t.Fatalf("source %T is not wrapped", s)
	}
	switch a := wrapped.Source.(type) {
	case *workday:
		return a.keyPrefix
	case *oracleCE:
		return a.keyPrefix
	case *ukg:
		return a.keyPrefix
	case *eightfold:
		return a.keyPrefix
	case *zwayam:
		return a.keyPrefix
	case *ibm:
		return a.keyPrefix
	case *hrone:
		return a.keyPrefix
	}
	t.Fatalf("source %T has no keyPrefix", wrapped.Source)
	return ""
}

// TestVolatileParamsAffectNeitherIdentityNorStatePrefix is the whole point of
// the change. Each row edits ONLY the transport param — the edit a user makes
// after the vendor moves their board — and demands that all three "have I
// seen this board?" guards keep matching: the identity (source marker and
// health key), the state prefix (HasPostingPrefix), and the job-ID prefix
// (Seen).
func TestVolatileParamsAffectNeitherIdentityNorStatePrefix(t *testing.T) {
	tests := []struct {
		name   string
		source string
		before params.Map
		after  params.Map
	}{
		{
			name: "workday shard migration", source: "workday",
			before: params.Map{"host": "acme.wd3.myworkdayjobs.com", "tenant": "acme", "site": "jobs"},
			after:  params.Map{"host": "acme.wd103.myworkdayjobs.com", "tenant": "acme", "site": "jobs"},
		},
		{
			name: "oraclece pod moves region", source: "oraclece",
			before: params.Map{"host": "eofe.fa.us2.oraclecloud.com", "site": "CX_1"},
			after:  params.Map{"host": "eofe.fa.em3.oraclecloud.com", "site": "CX_1"},
		},
		{
			name: "ukg cluster renumbered", source: "ukg",
			before: params.Map{"host": "recruiting.ultipro.com", "tenant": "PRO1053PROC", "board": "d6eed263-4950-420d-b9f8-5b1a441c931e"},
			after:  params.Map{"host": "recruiting5.ultipro.com", "tenant": "PRO1053PROC", "board": "d6eed263-4950-420d-b9f8-5b1a441c931e"},
		},
		{
			name: "eightfold vanity host", source: "eightfold",
			before: params.Map{"host": "jobs.ericsson.com", "domain": "ericsson.com", "query": "Cradlepoint"},
			after:  params.Map{"host": "ericsson.eightfold.ai", "domain": "ericsson.com", "query": "Cradlepoint"},
		},
		{
			name: "zwayam career site rebrand", source: "zwayam",
			before: params.Map{"domain": "careers.cult.fit", "company_id": "15470"},
			after:  params.Map{"domain": "careers.cure.fit", "company_id": "15470"},
		},
		{
			name: "ibm renumbers its search collection", source: "ibm",
			before: params.Map{"appid": "careers", "scope": "careers2", "rc": "in"},
			after:  params.Map{"appid": "careers3", "scope": "careers4", "rc": "in"},
		},
		{
			name: "hrone regenerates the portal token", source: "hrone",
			before: params.Map{"domain_code": "addverb", "api_key": strings.Repeat("k", 32), "request_type": "UVozgs-AUV1ILPLBxDlf7A", "company_code": "P3WIS3az9HUC6WAJ9eS1Rg"},
			after:  params.Map{"domain_code": "addverb", "api_key": strings.Repeat("k", 32), "request_type": "ZZZZZZ-ZZZ1ILPLBxDlf7A", "company_code": "P3WIS3az9HUC6WAJ9eS1Rg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, err := New(tc.source, "Acme", tc.before, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			after, err := New(tc.source, "Acme", tc.after, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if Identity(before) != Identity(after) {
				t.Errorf("identity changed: %q -> %q", Identity(before), Identity(after))
			}
			if StatePrefix(before) != StatePrefix(after) {
				t.Errorf("state prefix changed: %q -> %q", StatePrefix(before), StatePrefix(after))
			}
			if keyPrefixOf(t, before) != keyPrefixOf(t, after) {
				t.Errorf("job-ID prefix changed: %q -> %q", keyPrefixOf(t, before), keyPrefixOf(t, after))
			}
			// The adapter's own job IDs and the package's state prefix are two
			// independent pieces of code that must produce the same string;
			// they disagreed for workday for months.
			if got, want := keyPrefixOf(t, before), StatePrefix(before); got != want {
				t.Errorf("job-ID prefix %q != StatePrefix %q", got, want)
			}
		})
	}
}

// TestStableParamsDoAffectIdentity is the other half. Dropping a coordinate
// is not free: two boards that merge share one history, and the user is then
// emailed nothing for the one that lost.
func TestStableParamsDoAffectIdentity(t *testing.T) {
	tests := []struct {
		name   string
		source string
		a      params.Map
		b      params.Map
	}{
		{
			name: "workday tenant", source: "workday",
			a: params.Map{"host": "acme.wd1.myworkdayjobs.com", "tenant": "acme", "site": "jobs"},
			b: params.Map{"host": "acme.wd1.myworkdayjobs.com", "tenant": "acmeuk", "site": "jobs"},
		},
		{
			name: "workday site", source: "workday",
			// Real pair from the catalog: Nike runs "nke" and "nke2" as two
			// separate boards on one tenant.
			a: params.Map{"host": "nike.wd1.myworkdayjobs.com", "tenant": "nike", "site": "nke"},
			b: params.Map{"host": "nike.wd1.myworkdayjobs.com", "tenant": "nike", "site": "nke2"},
		},
		{
			name: "icims regional tenants", source: "icims",
			// careersus-maxlinear and careersintl-maxlinear are two real,
			// separately configured MaxLinear boards. The icims host IS the
			// tenant, which is why it is deliberately not volatile.
			a: params.Map{"host": "careersus-maxlinear.icims.com"},
			b: params.Map{"host": "careersintl-maxlinear.icims.com"},
		},
		{
			name: "successfactors employer DNS", source: "successfactors",
			a: params.Map{"host": "jobs.sap.com"},
			b: params.Map{"host": "careers.moodys.com"},
		},
		{
			name: "keka tenant host", source: "keka",
			a: params.Map{"host": "squadrun.keka.com", "portal": "default", "identifier": "c750f148-70b8-4a21-868e-f891a1b2d818"},
			b: params.Map{"host": "other.keka.com", "portal": "default", "identifier": "c750f148-70b8-4a21-868e-f891a1b2d818"},
		},
		{
			name: "oraclece pod", source: "oraclece",
			a: params.Map{"host": "eofe.fa.us2.oraclecloud.com", "site": "CX"},
			b: params.Map{"host": "edbz.fa.us2.oraclecloud.com", "site": "CX"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(tc.source, "A", tc.a, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := New(tc.source, "B", tc.b, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if Identity(a) == Identity(b) {
				t.Errorf("distinct boards collided at identity %q", Identity(a))
			}
			if StatePrefix(a) == StatePrefix(b) {
				t.Errorf("distinct boards collided at state prefix %q", StatePrefix(a))
			}
		})
	}
}
