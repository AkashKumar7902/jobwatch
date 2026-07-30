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

func identityFor(name string, p params.Map) string {
	switch name {
	case "greenhouse":
		return fmt.Sprintf("greenhouse/%s/%s", p.GetDefault("region", "us"), p.Get("board_token"))
	case "lever":
		return fmt.Sprintf("lever/%s/%s", p.GetDefault("region", "us"), p.Get("site"))
	case "ashby":
		return "ashby/" + p.Get("board_name")
	case "workable":
		return "workable/" + p.Get("account")
	case "recruitee":
		return "recruitee/" + p.Get("company_slug")
	case "rippling":
		return "rippling/" + p.Get("board_slug")
	case "polymer":
		return "polymer/" + p.Get("organization_slug")
	case "darwinbox":
		return "darwinbox/" + strings.ToLower(strings.TrimSpace(p.Get("subdomain")))
	case "freshteam":
		return "freshteam/" + p.Get("company_slug")
	case "smartrecruiters":
		return "smartrecruiters/" + p.Get("company_id")
	case "bamboohr":
		return "bamboohr/" + p.Get("company_slug")
	case "workday":
		return fmt.Sprintf("workday/%s/%s/%s", p.Get("host"), p.Get("tenant"), p.Get("site"))
	case "wayfair":
		return "wayfair"
	case "talentbrew":
		return "talentbrew/" + p.Get("host")
	case "amazon":
		return "amazon/" + strings.ToUpper(p.GetDefault("country_code", "IND"))
	case "auzmor":
		return "auzmor/" + strings.ToLower(strings.TrimSpace(p.Get("domain")))
	case "google":
		return "google/IN"
	case "atlassian", "deshaw", "medianet", "enphase", "richpanel", "makemytrip",
		"highergs", "hyperverge", "juspay", "komprise", "nationwithnamo",
		"nutanix", "ofbusiness", "publicissapient", "slb":
		return name
	case "walmart":
		return "walmart/IN"
	case "avature":
		return fmt.Sprintf(
			"avature/%s/%s/%s",
			p.Get("host"),
			strings.Trim(p.Get("site"), "/"),
			strings.Trim(p.Get("search_path"), "/"),
		)
	case "eightfold":
		identity := fmt.Sprintf("eightfold/%s/%s/%s", p.Get("host"), p.Get("domain"), strings.TrimSpace(p.Get("location")))
		if query := strings.TrimSpace(p.Get("query")); query != "" {
			identity += "/query=" + url.QueryEscape(query)
		}
		return identity
	case "kula":
		return "kula/" + p.Get("account_name")
	case "icims", "successfactors", "mynexthire":
		host, _ := normalizeBoardHost(p.Get("host"))
		return name + "/" + host
	case "ibm":
		return fmt.Sprintf("ibm/%s/%s/%s", p.Get("appid"), p.Get("scope"), p.Get("rc"))
	case "hrone":
		return fmt.Sprintf(
			"hrone/%s/%s/%s",
			strings.ToLower(strings.TrimSpace(p.Get("domain_code"))),
			strings.TrimSpace(p.Get("request_type")),
			strings.TrimSpace(p.Get("company_code")),
		)
	case "jubilant":
		return "jubilant/" + strings.ToUpper(strings.TrimSpace(p.GetDefault("country_code", "IND")))
	case "jio":
		return "jio/" + jioIdentityFunctions(p)
	case "oraclece":
		host, _ := normalizeBoardHost(p.Get("host"))
		identity := fmt.Sprintf("oraclece/%s/%s", host, p.Get("site"))
		if location := strings.TrimSpace(p.Get("location")); location != "" {
			identity += "/" + location
		}
		return identity
	case "keka":
		return fmt.Sprintf("keka/%s/%s/%s", p.Get("host"), p.Get("portal"), strings.ToLower(p.Get("identifier")))
	case "ukg":
		host, _ := normalizeBoardHost(p.Get("host"))
		board, _ := canonicalUKGUUID(p.Get("board"))
		return fmt.Sprintf("ukg/%s/%s/%s", host, p.Get("tenant"), board)
	case "zwayam":
		domain, _ := normalizeBoardHost(p.Get("domain"))
		companyID, _, _ := canonicalZwayamDecimal(p.Get("company_id"))
		return fmt.Sprintf("zwayam/%s/%s", domain, companyID)
	case "fastenal", "siemensjobs", "nykaa", "airoha", "payu", "forty2gears":
		return name
	}

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

func statePrefixFor(name string, p params.Map) string {
	switch name {
	case "greenhouse":
		return "greenhouse/" + p.Get("board_token") + "/"
	case "lever":
		return "lever/" + p.Get("site") + "/"
	case "ashby":
		return "ashby/" + p.Get("board_name") + "/"
	case "workable":
		return "workable/" + p.Get("account") + "/"
	case "recruitee":
		return "recruitee/" + p.Get("company_slug") + "/"
	case "rippling":
		return "rippling/" + p.Get("board_slug") + "/"
	case "polymer":
		return "polymer/" + p.Get("organization_slug") + "/"
	case "darwinbox":
		return "darwinbox/" + strings.ToLower(strings.TrimSpace(p.Get("subdomain"))) + "/"
	case "freshteam":
		return "freshteam/" + p.Get("company_slug") + "/"
	case "smartrecruiters":
		return "smartrecruiters/" + p.Get("company_id") + "/"
	case "bamboohr":
		return "bamboohr/" + p.Get("company_slug") + "/"
	case "workday":
		base := fmt.Sprintf("https://%s/wday/cxs/%s/%s", p.Get("host"), p.Get("tenant"), p.Get("site"))
		return "workday/" + base + "/"
	case "wayfair":
		return "wayfair/"
	case "talentbrew":
		return "talentbrew/" + p.Get("host") + "/"
	case "amazon":
		return "amazon/" + strings.ToUpper(p.GetDefault("country_code", "IND")) + "/"
	case "auzmor":
		return "auzmor/" + strings.ToLower(strings.TrimSpace(p.Get("domain"))) + "/"
	case "atlassian", "deshaw", "medianet", "enphase", "richpanel", "makemytrip",
		"highergs", "hyperverge", "juspay", "komprise", "nutanix", "ofbusiness",
		"publicissapient", "slb", "walmart", "google":
		return name + "/"
	case "nationwithnamo":
		return "nationwithnamo/gilp-impact-fellowship/"
	case "avature":
		return fmt.Sprintf("avature/%s/%s/", p.Get("host"), strings.Trim(p.Get("site"), "/"))
	case "eightfold":
		return fmt.Sprintf("eightfold/%s/%s/", p.Get("host"), p.Get("domain"))
	case "kula":
		return "kula/" + p.Get("account_name") + "/"
	case "icims", "successfactors", "mynexthire":
		host, _ := normalizeBoardHost(p.Get("host"))
		return name + "/" + host + "/"
	case "ibm":
		return fmt.Sprintf("ibm/%s/%s/%s/", p.Get("appid"), p.Get("scope"), p.Get("rc"))
	case "hrone":
		return fmt.Sprintf(
			"hrone/%s/%s/%s/",
			strings.ToLower(strings.TrimSpace(p.Get("domain_code"))),
			strings.TrimSpace(p.Get("request_type")),
			strings.TrimSpace(p.Get("company_code")),
		)
	case "jubilant":
		return "jubilant/" + strings.ToUpper(strings.TrimSpace(p.GetDefault("country_code", "IND"))) + "/"
	case "jio":
		return "jio/"
	case "oraclece":
		host, _ := normalizeBoardHost(p.Get("host"))
		return fmt.Sprintf("oraclece/%s/%s/", host, p.Get("site"))
	case "keka":
		return fmt.Sprintf("keka/%s/%s/", p.Get("host"), p.Get("portal"))
	case "ukg":
		host, _ := normalizeBoardHost(p.Get("host"))
		board, _ := canonicalUKGUUID(p.Get("board"))
		return fmt.Sprintf("ukg/%s/%s/%s/", host, p.Get("tenant"), board)
	case "zwayam":
		domain, _ := normalizeBoardHost(p.Get("domain"))
		companyID, _, _ := canonicalZwayamDecimal(p.Get("company_id"))
		return fmt.Sprintf("zwayam/%s/%s/", domain, companyID)
	case "fastenal", "siemensjobs", "nykaa", "airoha", "payu", "forty2gears":
		return name + "/"
	}
	return ""
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
