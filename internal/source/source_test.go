package source

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

type adapterSpoofSource struct{}

func (adapterSpoofSource) Company() string                            { return "Acme" }
func (adapterSpoofSource) Fetch(context.Context) ([]model.Job, error) { return nil, nil }
func (adapterSpoofSource) Adapter() string                            { return "secret/query?token=value" }

func TestAdapterOnlyExposesRegisteredWrapperType(t *testing.T) {
	if got := Adapter(adapterSpoofSource{}); got != "custom" {
		t.Fatalf("Adapter(third-party source) = %q, want custom", got)
	}
	src, err := New("atlassian", "Atlassian", nil, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if got := Adapter(src); got != "atlassian" {
		t.Fatalf("Adapter(registered source) = %q, want atlassian", got)
	}
}

func TestSourcesAndMatchersDoNotUsePackageGlobalLogging(t *testing.T) {
	sourceDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	internal := filepath.Dir(sourceDir)
	for _, directory := range []string{filepath.Join(internal, "source"), filepath.Join(internal, "match")} {
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "log.Printf(") {
				t.Errorf("%s uses package-global log.Printf; emit a sealed diagnostic instead", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOperationalSignalPathsStayConnected(t *testing.T) {
	sourceDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"amazon", "atlassian", "avature", "deshaw", "eightfold", "kula", "smartrecruiters", "workday"} {
		data, err := os.ReadFile(filepath.Join(sourceDir, name+".go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "diagnostic.Cap(") {
			t.Errorf("%s cap path is not connected to the runner diagnostic", name)
		}
	}
	for _, name := range []string{"enphase", "google"} {
		data, err := os.ReadFile(filepath.Join(sourceDir, name+".go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "diagnostic.Retry(") {
			t.Errorf("%s retry path is not connected to the runner diagnostic", name)
		}
	}
	matchData, err := os.ReadFile(filepath.Join(filepath.Dir(sourceDir), "match", "llm.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(matchData), "diagnostic.Retry("); got != 2 {
		t.Errorf("llm retry signals = %d, want 2", got)
	}
}

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
	// The shard host is transport: it is in neither string, so a shard
	// migration is invisible to state.
	wantPrefix := "workday/acme/jobs/"
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

func TestOracleIdentityIncludesLocationScope(t *testing.T) {
	client := &http.Client{}
	global, err := New("oraclece", "Acme", params.Map{
		"host": "jobs.example.com", "site": "CX",
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	india, err := New("oraclece", "Acme India", params.Map{
		"host": "jobs.example.com", "site": "CX", "location": "India",
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if Identity(global) == Identity(india) {
		t.Fatalf("location-scoped boards collided at %q", Identity(global))
	}
	if got := Identity(india); got != "oraclece/jobs/CX/India" {
		t.Fatalf("Identity() = %q", got)
	}
	if StatePrefix(global) != StatePrefix(india) {
		t.Fatalf("location scope changed shared job-ID prefix: %q != %q", StatePrefix(global), StatePrefix(india))
	}
}

func TestAddedSourceIdentitiesAndStatePrefixes(t *testing.T) {
	tests := []struct {
		source      string
		params      params.Map
		operational params.Map
		wantID      string
		wantPrefix  string
	}{
		{"amazon", params.Map{"country_code": "IND"}, params.Map{"max_postings": "10"}, "amazon/IND", "amazon/IND/"},
		{"auzmor", params.Map{"domain": "letstransport"}, nil, "auzmor/letstransport", "auzmor/letstransport/"},
		{"atlassian", nil, params.Map{"max_postings": "10"}, "atlassian", "atlassian/"},
		{"avature", params.Map{"host": "jobs.ea.com", "site": "en_US", "search_path": "search-jobs"}, params.Map{"max_postings": "10"}, "avature/jobs.ea.com/en_US/search-jobs", "avature/jobs.ea.com/en_US/"},
		{"deshaw", nil, params.Map{"max_postings": "10"}, "deshaw", "deshaw/"},
		{"eightfold", params.Map{"host": "apply.careers.microsoft.com", "domain": "microsoft.com", "location": "India", "query": "Cradlepoint"}, params.Map{"max_postings": "10"}, "eightfold/microsoft.com/India/query=Cradlepoint", "eightfold/microsoft.com/"},
		{"kula", params.Map{"account_name": "cashfree"}, params.Map{"max_postings": "10"}, "kula/cashfree", "kula/cashfree/"},
		{"medianet", nil, params.Map{"max_postings": "10"}, "medianet", "medianet/"},
		{"enphase", nil, params.Map{"max_pages": "2"}, "enphase", "enphase/"},
		{"icims", params.Map{"host": "careersus-maxlinear.icims.com"}, params.Map{"max_pages": "2"}, "icims/careersus-maxlinear.icims.com", "icims/careersus-maxlinear.icims.com/"},
		{"ibm", params.Map{"appid": "careers", "scope": "careers2", "rc": "in"}, nil, "ibm/in", "ibm/in/"},
		{"jubilant", params.Map{"country_code": "IND"}, nil, "jubilant/IND", "jubilant/IND/"},
		{"oraclece", params.Map{"host": "iaziqy.fa.ocs.oraclecloud.com", "site": "CX_1"}, params.Map{"max_pages": "2"}, "oraclece/iaziqy/CX_1", "oraclece/iaziqy/CX_1/"},
		{"richpanel", nil, nil, "richpanel", "richpanel/"},
		{"successfactors", params.Map{"host": "jobs.sap.com"}, params.Map{"max_pages": "2"}, "successfactors/jobs.sap.com", "successfactors/jobs.sap.com/"},
		{"highergs", nil, params.Map{"max_postings": "10"}, "highergs", "highergs/"},
		{"google", nil, nil, "google/IN", "google/"},
		{"hyperverge", nil, nil, "hyperverge", "hyperverge/"},
		{"juspay", nil, nil, "juspay", "juspay/"},
		{"komprise", nil, nil, "komprise", "komprise/"},
		{"makemytrip", nil, nil, "makemytrip", "makemytrip/"},
		{"mynexthire", params.Map{"host": "sharechat.mynexthire.com"}, nil, "mynexthire/sharechat.mynexthire.com", "mynexthire/sharechat.mynexthire.com/"},
		{"ofbusiness", nil, nil, "ofbusiness", "ofbusiness/"},
		{"nutanix", nil, nil, "nutanix", "nutanix/"},
		{"nationwithnamo", nil, nil, "nationwithnamo", "nationwithnamo/gilp-impact-fellowship/"},
		{"polymer", params.Map{"organization_slug": "swym"}, nil, "polymer/swym", "polymer/swym/"},
		{"darwinbox", params.Map{"subdomain": "moneyview"}, params.Map{"max_postings": "100"}, "darwinbox/moneyview", "darwinbox/moneyview/"},
		{"publicissapient", nil, params.Map{"max_postings": "10"}, "publicissapient", "publicissapient/"},
		{"slb", nil, params.Map{"max_postings": "10"}, "slb", "slb/"},
		{"freshteam", params.Map{"company_slug": "kaleyra-talent"}, nil, "freshteam/kaleyra-talent", "freshteam/kaleyra-talent/"},
		{"jio", params.Map{"functions": "0222,0206,0210,0216"}, params.Map{"max_postings": "500"}, "jio/0206+0210+0216+0222", "jio/"},
		{"walmart", nil, nil, "walmart/IN", "walmart/"},
		{"ukg", params.Map{"host": "recruiting2.ultipro.com", "tenant": "PRO1053PROC", "board": "d6eed263-4950-420d-b9f8-5b1a441c931e"}, nil, "ukg/PRO1053PROC/d6eed263-4950-420d-b9f8-5b1a441c931e", "ukg/PRO1053PROC/d6eed263-4950-420d-b9f8-5b1a441c931e/"},
		{"zwayam", params.Map{"domain": "careers.cult.fit", "company_id": "15470"}, nil, "zwayam/15470", "zwayam/15470/"},
		{"keka", params.Map{"host": "squadrun.keka.com", "portal": "default", "identifier": "c750f148-70b8-4a21-868e-f891a1b2d818"}, nil, "keka/squadrun.keka.com/default/c750f148-70b8-4a21-868e-f891a1b2d818", "keka/squadrun.keka.com/default/"},
		{"fastenal", nil, nil, "fastenal", "fastenal/"},
		{"siemensjobs", nil, nil, "siemensjobs", "siemensjobs/"},
		{"nykaa", nil, nil, "nykaa", "nykaa/"},
		{"airoha", nil, nil, "airoha", "airoha/"},
		{"payu", nil, nil, "payu", "payu/"},
		{"forty2gears", nil, nil, "forty2gears", "forty2gears/"},
	}
	for _, tc := range tests {
		t.Run(tc.source+"/"+tc.wantID, func(t *testing.T) {
			base := params.Map{}
			for key, value := range tc.params {
				base[key] = value
			}
			withLimit := params.Map{}
			for key, value := range base {
				withLimit[key] = value
			}
			for key, value := range tc.operational {
				withLimit[key] = value
			}
			a, err := New(tc.source, "Original name", base, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := New(tc.source, "Renamed company", withLimit, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if got := Identity(a); got != tc.wantID {
				t.Errorf("Identity() = %q, want %q", got, tc.wantID)
			}
			if Identity(b) != Identity(a) {
				t.Errorf("operational params changed identity: %q != %q", Identity(b), Identity(a))
			}
			if got := StatePrefix(a); got != tc.wantPrefix {
				t.Errorf("StatePrefix() = %q, want %q", got, tc.wantPrefix)
			}
		})
	}
}

func TestIdentityUsesFactoryCanonicalCoordinates(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		noncanon  params.Map
		canonical params.Map
	}{
		{
			name: "icims host", source: "icims",
			noncanon:  params.Map{"host": "Careers.Example.COM."},
			canonical: params.Map{"host": "careers.example.com"},
		},
		{
			name: "successfactors host", source: "successfactors",
			noncanon:  params.Map{"host": "Jobs.Example.COM."},
			canonical: params.Map{"host": "jobs.example.com"},
		},
		{
			name: "oracle host and location", source: "oraclece",
			noncanon:  params.Map{"host": "Jobs.Example.COM.", "site": "CX", "location": " India "},
			canonical: params.Map{"host": "jobs.example.com", "site": "CX", "location": "India"},
		},
		{
			name: "ukg host and board", source: "ukg",
			noncanon: params.Map{
				"host": "Recruiting.Example.COM.", "tenant": "TENANT",
				"board": "D6EED263-4950-420D-B9F8-5B1A441C931E",
			},
			canonical: params.Map{
				"host": "recruiting.example.com", "tenant": "TENANT",
				"board": "d6eed263-4950-420d-b9f8-5b1a441c931e",
			},
		},
		{
			name: "mynexthire host", source: "mynexthire",
			noncanon:  params.Map{"host": "Careers.MyNextHire.COM."},
			canonical: params.Map{"host": "careers.mynexthire.com"},
		},
		{
			name: "avature paths", source: "avature",
			noncanon: params.Map{
				"host": "jobs.example.com", "site": "/en_US/careers/",
				"search_path": "/SearchJobs/",
			},
			canonical: params.Map{
				"host": "jobs.example.com", "site": "en_US/careers",
				"search_path": "SearchJobs",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, err := New(test.source, "Example", test.noncanon, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := New(test.source, "Renamed", test.canonical, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if Identity(a) != Identity(b) {
				t.Errorf("identities differ: %q != %q", Identity(a), Identity(b))
			}
			if StatePrefix(a) != StatePrefix(b) {
				t.Errorf("state prefixes differ: %q != %q", StatePrefix(a), StatePrefix(b))
			}
		})
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
