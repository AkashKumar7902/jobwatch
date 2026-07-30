package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func TestJubilantListsCountryCompletelyAndHydratesDetail(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") == "" {
			t.Errorf("headers Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/getAllJobs":
			mustJubilantJSON(t, w, jubilantFixtureList())
		case "/getJobDetails/41334":
			id := 41334
			mustJubilantJSON(t, w, jubilantDetailResponse{
				Country: "IND", JobPostingDate: "30/07/26",
				JobTitle: "SRS-Microbial\u00a0Control\u00a0Solution", Function: "Research & Development",
				Location: "Greater Noida, Uttar Pradesh", Company: "Jubilant Ingrevia Limited",
				DescriptionHTML: "<p>Build &amp; ship.</p><ul><li>Use Go.</li></ul>",
				JobOpeningID:    &id, Status: jubilantActiveStatus,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src, err := New("jubilant", "Jubilant Group India", params.Map{"country_code": "ind"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if Identity(src) != "jubilant/IND" || StatePrefix(src) != "jubilant/IND/" {
		t.Fatalf("identity=%q prefix=%q", Identity(src), StatePrefix(src))
	}
	impl := src.(*identifiedSource).Source.(*jubilant)
	impl.apiBase = server.URL
	impl.siteBase = "https://jubilantcareer.jubl.com"

	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %#v, want one India job", jobs)
	}
	want := model.Job{
		ID: "jubilant/IND/41334", Company: "Jubilant Group India",
		Title: "SRS-Microbial Control Solution", Location: "Greater Noida, Uttar Pradesh",
		URL: "https://jubilantcareer.jubl.com/jobprofile/41334/home",
	}
	if !reflect.DeepEqual(jobs[0], want) {
		t.Fatalf("job =\n%+v\nwant\n%+v", jobs[0], want)
	}
	if err := src.(Detailer).Detail(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs[0].Description != "Build & ship.\nUse Go." {
		t.Errorf("description = %q", jobs[0].Description)
	}
	if wantDate := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC); !jobs[0].PostedAt.Equal(wantDate) {
		t.Errorf("PostedAt = %v, want %v", jobs[0].PostedAt, wantDate)
	}
	if !reflect.DeepEqual(requests, []string{"/getAllJobs", "/getJobDetails/41334"}) {
		t.Errorf("requests = %v", requests)
	}
}

func TestJubilantRejectsInvalidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  params.Map
		wantErr string
	}{
		{name: "invalid country", params: params.Map{"country_code": "India"}, wantErr: "ISO-3"},
		{name: "empty country", params: params.Map{"country_code": " "}, wantErr: "ISO-3"},
		{name: "unknown setting", params: params.Map{"country_code": "IND", "host": "evil.example"}, wantErr: `unsupported param "host"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New("jubilant", "Jubilant", test.params, nil); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestJubilantRejectsIncompleteOrDriftedLists(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*jubilantListResponse)
		wantErr string
	}{
		{name: "missing jobs", mutate: func(r *jubilantListResponse) { r.JobList = nil }, wantErr: "omitted jobs"},
		{name: "missing countries", mutate: func(r *jubilantListResponse) { r.CountryList = nil }, wantErr: "required facet"},
		{name: "duplicate id", mutate: func(r *jubilantListResponse) {
			*r.JobList = append(*r.JobList, (*r.JobList)[0])
		}, wantErr: "duplicate jobId"},
		{name: "invalid id", mutate: func(r *jubilantListResponse) {
			(*r.JobList)[0].JobID = "../41334"
		}, wantErr: "invalid jobId"},
		{name: "country mismatch", mutate: func(r *jubilantListResponse) {
			(*r.JobList)[0].CountryName = "United States"
		}, wantErr: "inconsistent country"},
		{name: "unknown company", mutate: func(r *jubilantListResponse) {
			(*r.JobList)[0].Company = "Unknown"
		}, wantErr: "absent from companyList"},
		{name: "unknown function", mutate: func(r *jubilantListResponse) {
			(*r.JobList)[0].FunctionalArea = "Unknown"
		}, wantErr: "absent from functionList"},
		{name: "unknown location", mutate: func(r *jubilantListResponse) {
			(*r.JobList)[0].LocationDescription = "Unknown"
		}, wantErr: "absent from locationList"},
		{name: "duplicate company facet", mutate: func(r *jubilantListResponse) {
			*r.CompanyList = append(*r.CompanyList, (*r.CompanyList)[0])
		}, wantErr: "duplicate company"},
		{name: "bad function facet", mutate: func(r *jubilantListResponse) {
			(*r.FunctionList)[0].Description = "Different"
		}, wantErr: "inconsistent name"},
		{name: "selected country absent", mutate: func(r *jubilantListResponse) {
			*r.CountryList = (*r.CountryList)[1:]
		}, wantErr: "absent from countryList"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := jubilantFixtureList()
			test.mutate(&response)
			_, err := jubilantFetchFixture(t, response)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestJubilantRejectsDriftedDetails(t *testing.T) {
	validID := 41334
	valid := jubilantDetailResponse{
		Country: "IND", JobPostingDate: "30/07/26",
		JobTitle: "SRS-Microbial Control Solution", Function: "Research & Development",
		Location: "Greater Noida, Uttar Pradesh", Company: "Jubilant Ingrevia Limited",
		DescriptionHTML: "<p>Build systems.</p>", JobOpeningID: &validID, Status: jubilantActiveStatus,
	}
	tests := []struct {
		name    string
		mutate  func(*jubilantDetailResponse)
		wantErr string
	}{
		{name: "missing id", mutate: func(d *jubilantDetailResponse) { d.JobOpeningID = nil }, wantErr: "jobOpeningId"},
		{name: "changed id", mutate: func(d *jubilantDetailResponse) { other := 7; d.JobOpeningID = &other }, wantErr: "jobOpeningId"},
		{name: "inactive", mutate: func(d *jubilantDetailResponse) { d.Status = "020" }, wantErr: "status"},
		{name: "country", mutate: func(d *jubilantDetailResponse) { d.Country = "USA" }, wantErr: "country"},
		{name: "title", mutate: func(d *jubilantDetailResponse) { d.JobTitle = "Other" }, wantErr: "title"},
		{name: "location", mutate: func(d *jubilantDetailResponse) { d.Location = "Other" }, wantErr: "location"},
		{name: "company", mutate: func(d *jubilantDetailResponse) { d.Company = "" }, wantErr: "company or function"},
		{name: "description", mutate: func(d *jubilantDetailResponse) { d.DescriptionHTML = "<p> </p>" }, wantErr: "empty jobdescr"},
		{name: "date", mutate: func(d *jubilantDetailResponse) { d.JobPostingDate = "2026-07-30" }, wantErr: "invalid jobpostingdate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := valid
			test.mutate(&detail)
			err := jubilantDetailFixture(t, detail)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func jubilantFetchFixture(t *testing.T, response jubilantListResponse) ([]model.Job, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mustJubilantJSON(t, w, response)
	}))
	defer server.Close()
	src, err := New("jubilant", "Jubilant", params.Map{"country_code": "IND"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	src.(*identifiedSource).Source.(*jubilant).apiBase = server.URL
	return src.Fetch(context.Background())
}

func jubilantDetailFixture(t *testing.T, detail jubilantDetailResponse) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mustJubilantJSON(t, w, detail)
	}))
	defer server.Close()
	src, err := New("jubilant", "Jubilant", params.Map{"country_code": "IND"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	src.(*identifiedSource).Source.(*jubilant).apiBase = server.URL
	job := model.Job{
		ID: "jubilant/IND/41334", Company: "Jubilant",
		Title: "SRS-Microbial Control Solution", Location: "Greater Noida, Uttar Pradesh",
	}
	return src.(Detailer).Detail(context.Background(), &job)
}

func jubilantFixtureList() jubilantListResponse {
	jobs := []jubilantListJob{
		{
			JobID: "41334", JobCountry: "IND", CountryName: "India",
			FunctionalArea: "Research & Development", JobTitle: "SRS-Microbial\u00a0Control\u00a0Solution",
			LocationDescription: "Greater Noida, Uttar Pradesh", Company: "Jubilant Ingrevia Limited",
		},
		{
			JobID: "50001", JobCountry: "USA", CountryName: "United States",
			FunctionalArea: "Sales", JobTitle: "Sales Manager",
			LocationDescription: "Boston, Massachusetts", Company: "Jubilant Radiopharma",
		},
	}
	companies := []jubilantCompany{
		{Code: "JVL", Name: "Jubilant Ingrevia Limited"},
		{Code: "JDR", Name: "Jubilant Radiopharma"},
	}
	countries := []jubilantCountry{{Name: "India", Code: "IND"}, {Name: "United States", Code: "USA"}}
	functions := []jubilantFunction{
		{Description: "Research & Development", Name: "Research & Development"},
		{Description: "Sales", Name: "Sales"},
	}
	locations := []jubilantLocation{
		{Description: "Greater Noida, Uttar Pradesh", Name: "Greater Noida, Uttar Pradesh"},
		{Description: "Boston, Massachusetts", Name: "Boston, Massachusetts"},
	}
	return jubilantListResponse{
		JobList: &jobs, CompanyList: &companies, CountryList: &countries,
		FunctionList: &functions, LocationList: &locations,
	}
}

func mustJubilantJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
