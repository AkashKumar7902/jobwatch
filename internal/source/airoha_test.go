package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAirohaFetchesAllPagesWithEnglishCultureAndDetails(t *testing.T) {
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = customTestUUID(i + 1)
	}
	cultureCalls := 0
	listCalls := 0
	detailCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Home/SetLanguage":
			cultureCalls++
			if r.URL.Query().Get("culture") != "en-US" || r.URL.Query().Get("returnUrl") != "/Jobs" {
				t.Errorf("SetLanguage query = %v", r.URL.Query())
			}
			http.SetCookie(w, &http.Cookie{
				Name: "hrisweb.test", Value: "c%3Den-US%7Cuic%3Den-US", Path: "/",
			})
			w.WriteHeader(http.StatusFound)
		case "/Jobs":
			listCalls++
			assertAirohaCulture(t, r)
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			start := (page - 1) * airohaPageSize
			end := min(start+airohaPageSize, len(ids))
			fmt.Fprint(w, `<table><tbody>`)
			for i := start; i < end; i++ {
				fmt.Fprint(w, airohaTestRow(ids[i], fmt.Sprintf("A2600%02d-Engineer", i)))
			}
			fmt.Fprint(w, `</tbody></table>`)
			if page == 1 {
				fmt.Fprint(w, `<a href="/Jobs?page=2">2</a>`)
			}
		case "/Jobs/Detail":
			detailCalls++
			assertAirohaCulture(t, r)
			if r.URL.Query().Get("sn") != ids[0] {
				t.Errorf("detail sn = %q", r.URL.Query().Get("sn"))
			}
			fmt.Fprint(w, `<h1>A260000-Engineer</h1>`+
				`<table><tr><td data-label="Location"> Taiwan / Hsinchu </td>`+
				`<td data-label="Type"> Full-time </td></tr></table>`+
				`<h3 class="w3-fit-text">Description</h3><div></div>`+
				`<p>Design SerDes.<BR>Own implementation.</p>`+
				`<h3 class="w3-fit-text">Requirement</h3><div></div>`+
				`<p>Master degree.<BR>Three years.</p>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &airoha{company: "Airoha", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 11 || cultureCalls != 1 || listCalls != 2 || detailCalls != 0 {
		t.Fatalf(
			"Fetch returned %d jobs; culture=%d list=%d detail=%d",
			len(jobs), cultureCalls, listCalls, detailCalls,
		)
	}
	first := &jobs[0]
	if first.ID != "airoha/"+ids[0] || first.Title != "A260000-Engineer" ||
		first.Location != "Taiwan / Hsinchu" {
		t.Errorf("first job = %+v", *first)
	}
	if err := src.Detail(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if cultureCalls != 1 || detailCalls != 1 {
		t.Errorf("culture=%d detail=%d", cultureCalls, detailCalls)
	}
	if first.EmploymentType != "Full-time" ||
		first.Description != "Design SerDes.\nOwn implementation.\n\nRequirement\nMaster degree.\nThree years." {
		t.Errorf("detailed job = %+v", *first)
	}
}

func TestAirohaRequiresCultureCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	src := &airoha{company: "Airoha", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}

func TestAirohaRejectsDuplicatePostingIDs(t *testing.T) {
	id := customTestUUID(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Home/SetLanguage" {
			http.SetCookie(w, &http.Cookie{
				Name: "hrisweb.test", Value: "c%3Den-US%7Cuic%3Den-US", Path: "/",
			})
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Fprint(w, `<table><tbody>`)
		fmt.Fprint(w, airohaTestRow(id, "One"))
		fmt.Fprint(w, airohaTestRow(id, "Duplicate"))
		fmt.Fprint(w, `</tbody></table>`)
	}))
	defer srv.Close()
	src := &airoha{company: "Airoha", base: srv.URL, client: srv.Client()}
	jobs, err := src.Fetch(context.Background())
	if err == nil || jobs != nil {
		t.Fatalf("Fetch = (%#v, %v), want nil jobs and error", jobs, err)
	}
}

func airohaTestRow(id, title string) string {
	cells := []string{
		title, "R&amp;D", "Taiwan / Hsinchu", "3 years", "Master",
		`<a href="/Jobs/Detail?sn=` + id + `">Detail</a>`,
	}
	var row strings.Builder
	row.WriteString("<tr>")
	for _, cell := range cells {
		row.WriteString("<td>")
		row.WriteString(cell)
		row.WriteString("</td>")
	}
	row.WriteString("</tr>")
	return row.String()
}

func assertAirohaCulture(t *testing.T, r *http.Request) {
	t.Helper()
	cookie, err := r.Cookie("hrisweb.test")
	if err != nil || cookie.Value != "c%3Den-US%7Cuic%3Den-US" {
		t.Errorf("culture cookie = %#v, %v", cookie, err)
	}
}
