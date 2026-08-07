// Package source fetches job postings from applicant tracking systems and
// first-party careers endpoints. Each adapter implements Source, registers a
// Factory in init(), and exposes stable board/job identities for state.
package source

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

// Source fetches the current openings of one company from one ATS.
type Source interface {
	// Company is the display name used in logs and notifications.
	Company() string
	// Fetch returns all currently open jobs, normalized.
	Fetch(ctx context.Context) ([]model.Job, error)
}

// identifiedSource adds a stable board identity to a Source. The identity is
// derived from connection params only (never the display name or operational
// limits), so renaming a company or changing max_postings does not make an
// existing board look new.
type identifiedSource struct {
	Source
	identity    string
	statePrefix string
}

func (s *identifiedSource) Identity() string    { return s.identity }
func (s *identifiedSource) StatePrefix() string { return s.statePrefix }

// Detail forwards the optional Detailer interface through the wrapper.
// Embedding only promotes the Source interface's methods, so without this
// the runner's `src.(Detailer)` assertion would fail for every wrapped
// source and lazy-detail boards would evaluate jobs with empty
// descriptions. Non-Detailer sources no-op (their Fetch already fills
// descriptions, so the runner never calls this for them).
func (s *identifiedSource) Detail(ctx context.Context, job *model.Job) error {
	if d, ok := s.Source.(Detailer); ok {
		return d.Detail(ctx, job)
	}
	return nil
}

// Identity returns the canonical ATS board identity for s. Sources created by
// New always have one; the Company fallback keeps hand-written test sources
// and third-party implementations compatible.
func Identity(s Source) string {
	if identified, ok := s.(interface{ Identity() string }); ok {
		return identified.Identity()
	}
	return "custom/" + s.Company()
}

// StatePrefix returns the prefix used by this source's stable model.Job IDs.
// It lets state migrations recognize an already-used board even when none of
// its currently open postings overlap the latest fetch.
func StatePrefix(s Source) string {
	if identified, ok := s.(interface{ StatePrefix() string }); ok {
		return identified.StatePrefix()
	}
	return ""
}

// Factory builds a Source for one company from its config entry.
type Factory func(company string, p params.Map, client *http.Client) (Source, error)

var registry = map[string]Factory{}

// Register makes a source type available under the given name.
// Call it from init() in the file implementing the source.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("source: duplicate registration of " + name)
	}
	registry[name] = f
}

// New builds the source type registered under name.
func New(name, company string, p params.Map, client *http.Client) (Source, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown source type %q (available: %v)", name, Names())
	}
	s, err := f(company, p, client)
	if err != nil {
		return nil, err
	}
	return &identifiedSource{
		Source: s, identity: identityFor(name, p), statePrefix: statePrefixFor(name, p),
	}, nil
}

// coord is one connection coordinate of a board, tagged with the role it
// plays in that board's identity.
//
// This type exists because identity used to be declared TWICE: identityFor
// and statePrefixFor were parallel switches over the same ~60 adapters, and
// nothing forced them to agree about which params name a board and which
// merely say how to reach it today. They drifted — for workday one produced
// "workday/{host}/{tenant}/{site}" while the other produced
// "workday/https://{host}/wday/cxs/{tenant}/{site}/" — and both baked in the
// vendor's mutable host, as did the job IDs. When an ATS moves a tenant
// between shards the user edits `host`, and all three "have I seen this
// board?" guards miss together: the marker key changes, the posting prefix
// changes, and every job ID changes. The board is silently re-baselined and
// its entire history is orphaned.
//
// So the roles are declared once, per adapter, and both strings are derived
// from that one declaration (see identityFor and statePrefixFor):
//
//	Identity    names the board across runs. It is what the source marker and
//	            the health record are keyed by.
//	StatePrefix is the namespace every job ID from this board starts with. It
//	            is what proves a board is already known even when none of its
//	            open postings survived from the last run.
//
// Volatile is the point of the whole exercise: it means TRANSPORT — the value
// says how to reach the board today, and the vendor can change it without the
// employer changing anything. A volatile coord appears in neither string and
// in no job ID. It is still declared here (rather than simply omitted) so that
// "which params are transport" stays a visible, testable fact about each
// adapter instead of an absence nobody can review.
type coord struct {
	Key   string // config param name, or a role name for a fixed value
	Value string // canonical value, exactly as it appears in the identity

	// Volatile: transport. Excluded from the identity AND the state prefix.
	Volatile bool
	// Namespace: appears in the state prefix, i.e. in every job ID.
	Namespace bool
	// PrefixOnly: in the state prefix but NOT in the identity. Exactly one
	// adapter needs this (nationwithnamo, whose job IDs carry a fixed program
	// path its identity never had), and it is kept only because changing that
	// board's identity would orphan its marker for no benefit.
	PrefixOnly bool
}

// stable: identifies the board and namespaces its jobs.
func stable(key, value string) coord { return coord{Key: key, Value: value, Namespace: true} }

// scoped: narrows the identity but NOT the job-ID namespace. Two configured
// views of one board (a region, a location filter, a search query) must not
// collide as identities, yet their postings live in the same namespace —
// which is exactly what lets an added location scope inherit history instead
// of re-baselining.
func scoped(key, value string) coord { return coord{Key: key, Value: value} }

// volatile: transport only. See coord.Volatile.
func volatile(key, value string) coord { return coord{Key: key, Value: value, Volatile: true} }

// prefixOnly: in the job-ID namespace but not in the identity.
func prefixOnly(key, value string) coord {
	return coord{Key: key, Value: value, Namespace: true, PrefixOnly: true}
}

// coordsFor declares one adapter's coordinates, in identity order. The second
// result is false for a name this file does not know, which is how externally
// registered sources still get a deterministic (if unnamespaced) identity.
//
// Adding an adapter means adding a case here — TestEveryRegisteredSourceIsDeclared
// fails otherwise — and the choice each case makes is a permanent commitment:
// changing a stable or namespace coord's shape later orphans that board's
// history exactly the way the host did.
func coordsFor(name string, p params.Map) ([]coord, bool) {
	switch name {
	case "greenhouse":
		// region picks the API host, but greenhouse board tokens are globally
		// unique, so it scopes the identity without splitting the namespace.
		return []coord{
			scoped("region", p.GetDefault("region", "us")),
			stable("board_token", p.Get("board_token")),
		}, true
	case "lever":
		return []coord{
			scoped("region", p.GetDefault("region", "us")),
			stable("site", p.Get("site")),
		}, true
	case "ashby":
		return []coord{stable("board_name", p.Get("board_name"))}, true
	case "workable":
		return []coord{stable("account", p.Get("account"))}, true
	case "recruitee":
		return []coord{stable("company_slug", p.Get("company_slug"))}, true
	case "rippling":
		return []coord{stable("board_slug", p.Get("board_slug"))}, true
	case "polymer":
		return []coord{stable("organization_slug", p.Get("organization_slug"))}, true
	case "darwinbox":
		// The subdomain IS the employer's tenant (moneyview.darwinbox.in),
		// not a shard. Not volatile.
		return []coord{stable("subdomain", strings.ToLower(strings.TrimSpace(p.Get("subdomain"))))}, true
	case "freshteam":
		return []coord{stable("company_slug", p.Get("company_slug"))}, true
	case "smartrecruiters":
		return []coord{stable("company_id", p.Get("company_id"))}, true
	case "bamboohr":
		return []coord{stable("company_slug", p.Get("company_slug"))}, true
	case "workday":
		// host is pure transport: "redhat.wd5.myworkdayjobs.com" says which of
		// Workday's ~8 shards serves this tenant TODAY. Workday migrates
		// tenants between shards (wd3 -> wd5 -> wd103 ...) and the employer's
		// board is unchanged. tenant+site are the employer's own coordinates
		// and appear in every URL the user ever pastes into config.
		return []coord{
			volatile("host", p.Get("host")),
			stable("tenant", p.Get("tenant")),
			stable("site", p.Get("site")),
		}, true
	case "wayfair":
		return nil, true
	case "talentbrew":
		// host here is the employer's own careers DNS (jobs.mytalentbrew...
		// style vanity domains). NOT volatile: it is the only coordinate this
		// adapter has, and dropping it would merge every talentbrew board.
		return []coord{stable("host", p.Get("host"))}, true
	case "amazon":
		return []coord{stable("country_code", strings.ToUpper(p.GetDefault("country_code", "IND")))}, true
	case "auzmor":
		// domain is the employer's Auzmor tenant slug, not a shard host.
		return []coord{stable("domain", strings.ToLower(strings.TrimSpace(p.Get("domain"))))}, true
	case "google":
		// The adapter is hard-wired to the India job feed. The country scopes
		// the identity; job IDs were never namespaced by it.
		return []coord{scoped("country", "IN")}, true
	case "walmart":
		return []coord{scoped("country", "IN")}, true
	case "atlassian", "deshaw", "medianet", "enphase", "richpanel", "makemytrip",
		"highergs", "hyperverge", "juspay", "komprise", "nutanix", "ofbusiness",
		"publicissapient", "slb", "fastenal", "siemensjobs", "nykaa", "airoha",
		"payu", "forty2gears":
		// One hand-written adapter per employer: the source name is the whole
		// coordinate system.
		return nil, true
	case "nationwithnamo":
		return []coord{prefixOnly("program", "gilp-impact-fellowship")}, true
	case "avature":
		// host is the employer's own careers domain (jobs.ea.com). search_path
		// only chooses which listing page to scrape, so it scopes without
		// splitting the namespace.
		return []coord{
			stable("host", p.Get("host")),
			stable("site", strings.Trim(p.Get("site"), "/")),
			scoped("search_path", strings.Trim(p.Get("search_path"), "/")),
		}, true
	case "eightfold":
		// host is Eightfold's serving domain and moves with their tenancy
		// (jobs.ericsson.com and morganstanley.eightfold.ai serve the same
		// kind of board); `domain` is the employer key Eightfold's own API is
		// queried with, and it is what survives a host move.
		//
		// location is always emitted, even empty: an empty component is what
		// today's identities contain, and the marker
		// "eightfold/jobs.ericsson.com/ericsson.com//query=Cradlepoint" — with
		// its double slash — is a real stored key.
		coords := []coord{
			volatile("host", p.Get("host")),
			stable("domain", p.Get("domain")),
			scoped("location", strings.TrimSpace(p.Get("location"))),
		}
		if query := strings.TrimSpace(p.Get("query")); query != "" {
			coords = append(coords, scoped("query", "query="+url.QueryEscape(query)))
		}
		return coords, true
	case "kula":
		return []coord{stable("account_name", p.Get("account_name"))}, true
	case "icims", "successfactors", "mynexthire":
		// Deliberately NOT volatile. For successfactors the host is the
		// employer's own DNS (jobs.sap.com, careers.moodys.com); for icims the
		// host IS the tenant, and careersus-maxlinear vs careersintl-maxlinear
		// are two real, separately configured boards that must stay distinct.
		// A host edit here loses that board's history — accepted: ~2% of
		// records, versus ~55% fixed deterministically by the volatile calls.
		host, _ := normalizeBoardHost(p.Get("host"))
		return []coord{stable("host", host)}, true
	case "ibm":
		// appid and scope name IBM's search collection ("careers"/"careers2").
		// They are Watson Discovery deployment coordinates that IBM has
		// already renumbered once; rc (the country) is the board.
		return []coord{
			volatile("appid", p.Get("appid")),
			volatile("scope", p.Get("scope")),
			stable("rc", p.Get("rc")),
		}, true
	case "hrone":
		// request_type is an opaque per-portal token HROne regenerates when a
		// customer reconfigures their career portal; domain_code + company_code
		// are the employer.
		return []coord{
			stable("domain_code", strings.ToLower(strings.TrimSpace(p.Get("domain_code")))),
			volatile("request_type", strings.TrimSpace(p.Get("request_type"))),
			stable("company_code", strings.TrimSpace(p.Get("company_code"))),
		}, true
	case "jubilant":
		return []coord{stable("country_code", strings.ToUpper(strings.TrimSpace(p.GetDefault("country_code", "IND"))))}, true
	case "jio":
		// The function filter narrows which postings we look at, not which
		// board they came from: one Jio careers site, one namespace.
		return []coord{scoped("functions", jioIdentityFunctions(p))}, true
	case "oraclece":
		// Only the DNS SUFFIX is volatile. An Oracle CE host looks like
		// "eofe.fa.us2.oraclecloud.com": the first label is the customer pod
		// and is as stable as the tenant, while ".fa.us2.oraclecloud.com" is
		// the data-centre region Oracle moves pods between.
		label, suffix := splitHostLabel(p.Get("host"))
		coords := []coord{
			stable("host_pod", label),
			volatile("host_suffix", suffix),
			stable("site", p.Get("site")),
		}
		if location := strings.TrimSpace(p.Get("location")); location != "" {
			coords = append(coords, scoped("location", location))
		}
		return coords, true
	case "keka":
		// Keka hosts are per-employer subdomains (squadrun.keka.com): the host
		// is the tenant, so it stays stable.
		return []coord{
			stable("host", p.Get("host")),
			stable("portal", p.Get("portal")),
			scoped("identifier", strings.ToLower(p.Get("identifier"))),
		}, true
	case "ukg":
		// recruiting.ultipro.com / recruiting2.ultipro.com / recruiting5... are
		// UKG's own numbered clusters; the tenant code is the employer.
		host, _ := normalizeBoardHost(p.Get("host"))
		board, _ := canonicalUKGUUID(p.Get("board"))
		return []coord{
			volatile("host", host),
			stable("tenant", p.Get("tenant")),
			stable("board", board),
		}, true
	case "zwayam":
		// The career-site domain is the employer's marketing DNS and gets
		// re-pointed on rebrands; company_id is Zwayam's own account number
		// and never changes.
		domain, _ := normalizeBoardHost(p.Get("domain"))
		companyID, _, _ := canonicalZwayamDecimal(p.Get("company_id"))
		return []coord{
			volatile("domain", domain),
			stable("company_id", companyID),
		}, true
	}
	return nil, false
}

// splitHostLabel splits a hostname into its first DNS label and the rest.
// normalizeBoardHost guarantees at least one dot for every adapter that uses
// this; a dotless value degrades to "all label, no suffix" rather than
// panicking, because identity must never depend on a validation the factory
// already performed.
func splitHostLabel(host string) (label, suffix string) {
	host, err := normalizeBoardHost(host)
	if err != nil {
		return "", ""
	}
	label, suffix, _ = strings.Cut(host, ".")
	return label, suffix
}

// identityFor derives the board identity from the declared coordinates. It is
// a projection of coordsFor, not a second declaration: that is the whole
// point of coord.
func identityFor(name string, p params.Map) string {
	coords, known := coordsFor(name, p)
	if !known {
		// Keep future externally registered sources deterministic too.
		keys := make([]string, 0, len(p))
		for key := range p {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var parts []string
		for _, key := range keys {
			parts = append(parts, key+"="+p[key])
		}
		return name + "/" + strings.Join(parts, ",")
	}
	parts := make([]string, 0, len(coords))
	for _, c := range coords {
		if c.Volatile || c.PrefixOnly {
			continue
		}
		parts = append(parts, c.Value)
	}
	if len(parts) == 0 {
		return name
	}
	return name + "/" + strings.Join(parts, "/")
}

// statePrefixFor derives the job-ID namespace from the same declaration. The
// empty string means "this source type does not namespace its job IDs", which
// the runner treats as "cannot prove this board is new".
func statePrefixFor(name string, p params.Map) string {
	coords, known := coordsFor(name, p)
	if !known {
		return ""
	}
	parts := make([]string, 0, len(coords))
	for _, c := range coords {
		if c.Volatile || !c.Namespace {
			continue
		}
		parts = append(parts, c.Value)
	}
	if len(parts) == 0 {
		return name + "/"
	}
	return name + "/" + strings.Join(parts, "/") + "/"
}

// Detailer is implemented by sources whose list endpoint lacks the full
// posting (description, employment type). The runner calls Detail only for
// jobs it actually evaluates — new or retried ones — so whole boards can be
// listed completely without paying one request per posting per run.
type Detailer interface {
	Detail(ctx context.Context, job *model.Job) error
}

// Names lists registered source types, sorted, for error messages.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
