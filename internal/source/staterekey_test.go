package source

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"jobwatch/internal/config"
	"jobwatch/internal/params"
)

// legacySuffixes are per-adapter posting suffixes shaped like the real thing:
// the rules' idempotency arguments depend on whether a suffix can contain a
// slash, so a generic "1" would test less than it looks like it does.
var legacySuffixes = map[string]string{
	"workday":   "job/Pune/Software-Engineer_R-12345",
	"oraclece":  "1290",
	"ukg":       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	"eightfold": "9000000000000001",
	"zwayam":    "98765",
	"ibm":       "120000BR",
	"hrone":     "encrypted_position_001",
}

func legacySuffix(srcType string) string {
	if s, ok := legacySuffixes[srcType]; ok {
		return s
	}
	return "posting-1"
}

// legacyKey rebuilds the job ID an adapter WOULD have produced before the
// transport params were removed from job IDs. For the adapters that never
// embedded transport it is simply today's key, which is the correct legacy
// key for them: nothing about their format changed.
func legacyKey(t *testing.T, srcType string, p params.Map, s Source) string {
	t.Helper()
	suffix := legacySuffix(srcType)
	switch srcType {
	case "workday":
		return fmt.Sprintf("workday/https://%s/wday/cxs/%s/%s/%s",
			p.Get("host"), p.Get("tenant"), p.Get("site"), suffix)
	case "oraclece":
		host, _ := normalizeBoardHost(p.Get("host"))
		return fmt.Sprintf("oraclece/%s/%s/%s", host, p.Get("site"), suffix)
	case "ukg":
		host, _ := normalizeBoardHost(p.Get("host"))
		board, _ := canonicalUKGUUID(p.Get("board"))
		return fmt.Sprintf("ukg/%s/%s/%s/%s", host, p.Get("tenant"), board, suffix)
	case "eightfold":
		return fmt.Sprintf("eightfold/%s/%s/%s", p.Get("host"), p.Get("domain"), suffix)
	case "zwayam":
		domain, _ := normalizeBoardHost(p.Get("domain"))
		companyID, _, _ := canonicalZwayamDecimal(p.Get("company_id"))
		return fmt.Sprintf("zwayam/%s/%s/%s", domain, companyID, suffix)
	case "ibm":
		return fmt.Sprintf("ibm/%s/%s/%s/%s", p.Get("appid"), p.Get("scope"), p.Get("rc"), suffix)
	case "hrone":
		return fmt.Sprintf("hrone/%s/%s/%s/%s",
			strings.ToLower(strings.TrimSpace(p.Get("domain_code"))),
			strings.TrimSpace(p.Get("request_type")),
			strings.TrimSpace(p.Get("company_code")), suffix)
	}
	if prefix := StatePrefix(s); prefix != "" {
		return prefix + suffix
	}
	t.Fatalf("source type %q has no state prefix", srcType)
	return ""
}

// catalogSources builds every board in the live catalog once.
func catalogSources(t *testing.T) []struct {
	Company string
	Type    string
	Params  params.Map
	Source  Source
} {
	t.Helper()
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out []struct {
		Company string
		Type    string
		Params  params.Map
		Source  Source
	}
	for _, c := range cfg.Companies {
		s, err := New(c.Source, c.Name, c.Params, &http.Client{})
		if err != nil {
			t.Fatalf("company %q: %v", c.Name, err)
		}
		out = append(out, struct {
			Company string
			Type    string
			Params  params.Map
			Source  Source
		}{c.Name, c.Source, c.Params, s})
	}
	return out
}

// TestRekeyMatchesLiveStatePrefixes is the test that would have caught the
// original drift. For every board in the catalog it synthesizes the OLD key,
// runs the migration rule over it, and demands the result be exactly what
// StatePrefix says today plus the untouched posting suffix.
//
// The two sides come from unrelated code — a frozen regex on one side, the
// coordinate table on the other — so if anybody ever edits one, this fails
// instead of the watcher silently re-baselining 47% of its state.
func TestRekeyMatchesLiveStatePrefixes(t *testing.T) {
	seenTypes := map[string]bool{}
	for _, board := range catalogSources(t) {
		seenTypes[board.Type] = true
		old := legacyKey(t, board.Type, board.Params, board.Source)
		want := StatePrefix(board.Source) + legacySuffix(board.Type)
		if got := RekeyStateKey(old); got != want {
			t.Errorf("%s (%s):\n  old  %q\n  got  %q\n  want %q", board.Company, board.Type, old, got, want)
		}
	}
	for _, srcType := range []string{"workday", "oraclece", "ukg", "eightfold", "zwayam", "ibm", "hrone"} {
		if !seenTypes[srcType] {
			t.Errorf("catalog no longer exercises %q; the rule for it is now untested against real params", srcType)
		}
	}
}

// TestRekeyRulesAreIdempotent is the property the "no schema version" design
// rests on. The migration is not gated by a flag or a version stamp: it runs
// at the top of every cycle, forever. That is only safe if every rule's
// output lies outside its own input language.
//
// The corpus is deliberately broad: every rule's output for every real board,
// every current-format key, the reserved runner keys, and hand-written
// adversarial shapes.
func TestRekeyRulesAreIdempotent(t *testing.T) {
	var corpus []string
	for _, board := range catalogSources(t) {
		old := legacyKey(t, board.Type, board.Params, board.Source)
		// The output of a rule...
		corpus = append(corpus, RekeyStateKey(old))
		// ...and a key already written in today's format.
		corpus = append(corpus, StatePrefix(board.Source)+legacySuffix(board.Type))
	}
	corpus = append(corpus,
		// Reserved runner keys are never migrated: markers are a derived
		// cache, and a real one is unparseable anyway (note the double slash).
		"__jobwatch_source__/eightfold/jobs.ericsson.com/ericsson.com//query=Cradlepoint",
		"__jobwatch_source__/workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/2",
		"__jobwatch_source_seed_in_progress__/workday/redhat/jobs",
		"__jobwatch_health__/workday/redhat/jobs",
		"__jobwatch_health__/@run",
		"__jobwatch_source_registry_v2__",
		// Adapters that never embedded transport.
		"greenhouse/stripe/4321",
		"lever/mixpanel/abcd-ef01",
		"icims/careersus-maxlinear.icims.com/job/2026-1234/engineer",
		"successfactors/jobs.sap.com/12345",
		"talentbrew/jobs.dell.com/12345",
		"keka/squadrun.keka.com/default/abc",
		"darwinbox/moneyview/xyz",
		// Output shapes that superficially resemble their own inputs.
		"workday/https:/wday/cxs/acme/jobs/job/1",
		"workday/redhat/jobs/job/Bengaluru/Engineer_R-1",
		"oraclece/eofe/CX_1/1290",
		"ukg/PRO1053PROC/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/1",
		"eightfold/ericsson.com/9000000000000001",
		"zwayam/15470/98765",
		"ibm/in/120000BR",
		"hrone/addverb/P3WIS3az9HUC6WAJ9eS1Rg/encrypted_position_001",
		// Junk keys are dropped, never moved: leaving them alone here is what
		// keeps the two operations independent.
		"workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/2",
	)
	for _, key := range corpus {
		if got := RekeyStateKey(key); got != key {
			t.Errorf("second pass rewrote a key:\n  in  %q\n  out %q", key, got)
		}
	}
}

// TestRekeyStateKeyRewritesTheSevenFamilies pins the exact input/output pairs
// the rules were frozen on, including the shapes measured in the live state
// file.
func TestRekeyStateKeyRewritesTheSevenFamilies(t *testing.T) {
	tests := []struct {
		name string
		old  string
		want string
	}{
		{
			"workday keeps the whole external path",
			"workday/https://redhat.wd5.myworkdayjobs.com/wday/cxs/redhat/jobs/job/Bengaluru/Senior_R-1234",
			"workday/redhat/jobs/job/Bengaluru/Senior_R-1234",
		},
		{
			"workday shard number is irrelevant",
			"workday/https://accenture.wd103.myworkdayjobs.com/wday/cxs/accenture/AccentureCareers/job/X",
			"workday/accenture/AccentureCareers/job/X",
		},
		{
			"oraclece keeps the pod, drops the region",
			"oraclece/eofe.fa.us2.oraclecloud.com/BNY-Careers/12345",
			"oraclece/eofe/BNY-Careers/12345",
		},
		{
			"oraclece short vendor host",
			"oraclece/jpmc.fa.oraclecloud.com/CX_1001/999",
			"oraclece/jpmc/CX_1001/999",
		},
		{
			"ukg drops the cluster host",
			"ukg/recruiting2.ultipro.com/PRO1053PROC/d6eed263-4950-420d-b9f8-5b1a441c931e/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"ukg/PRO1053PROC/d6eed263-4950-420d-b9f8-5b1a441c931e/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
		{
			"eightfold drops the serving host",
			"eightfold/jobs.ericsson.com/ericsson.com/9000000000000001",
			"eightfold/ericsson.com/9000000000000001",
		},
		{
			"zwayam drops the career-site domain",
			"zwayam/careers.cult.fit/15470/98765",
			"zwayam/15470/98765",
		},
		{
			"ibm drops appid and scope",
			"ibm/careers/careers2/in/120000BR",
			"ibm/in/120000BR",
		},
		{
			"hrone drops request_type from the middle",
			"hrone/addverb/UVozgs-AUV1ILPLBxDlf7A/P3WIS3az9HUC6WAJ9eS1Rg/encrypted_position_001",
			"hrone/addverb/P3WIS3az9HUC6WAJ9eS1Rg/encrypted_position_001",
		},
		{
			"an unrelated adapter is untouched",
			"greenhouse/stripe/4321",
			"greenhouse/stripe/4321",
		},
		{
			"a marker naming an old identity is untouched",
			"__jobwatch_source__/workday/https://redhat.wd5.myworkdayjobs.com/wday/cxs/redhat/jobs",
			"__jobwatch_source__/workday/https://redhat.wd5.myworkdayjobs.com/wday/cxs/redhat/jobs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RekeyStateKey(tc.old); got != tc.want {
				t.Errorf("RekeyStateKey(%q)\n = %q\nwant %q", tc.old, got, tc.want)
			}
		})
	}
}

// TestJunkBareBaseKeysAreDropped covers the 18 keys that are a workday board
// base with no posting after it. They must be recognized as junk, must NOT be
// moved (a bare board-prefix key would read as "this board has history" and
// defeat the census), and nothing else may be caught by the predicate.
func TestJunkBareBaseKeysAreDropped(t *testing.T) {
	junk := []string{
		"workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/2",
		"workday/https://barclays.wd3.myworkdayjobs.com/wday/cxs/barclays/External_Career_Site_Barclays",
	}
	keep := []string{
		// A real posting under the same board.
		"workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/2/job/Pune/Engineer",
		// The migrated form of a board prefix is not matched either: the
		// predicate is scoped to the exact legacy shape.
		"workday/citi/2",
		"workday/citi/2/job/Pune/Engineer",
		// Reserved keys are the runner's, not ours.
		"__jobwatch_source__/workday/https://citi.wd5.myworkdayjobs.com/wday/cxs/citi/2",
		// Other adapters.
		"greenhouse/stripe/4321",
		"oraclece/eofe/BNY-Careers/12345",
	}
	for _, id := range junk {
		if !IsJunkStateKey(id) {
			t.Errorf("IsJunkStateKey(%q) = false, want true", id)
		}
		if got := RekeyStateKey(id); got != id {
			t.Errorf("junk key was moved instead of dropped: %q -> %q", id, got)
		}
	}
	for _, id := range keep {
		if IsJunkStateKey(id) {
			t.Errorf("IsJunkStateKey(%q) = true, want false", id)
		}
	}
}
