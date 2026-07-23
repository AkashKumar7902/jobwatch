package catalog_test

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"

	"jobwatch/internal/config"
	"jobwatch/internal/params"
	"jobwatch/internal/source"
)

type auditRow struct {
	Ordinal     string
	Name        string
	Disposition string
	Source      string
	ParamsJSON  string
}

func readAudit(t *testing.T) []auditRow {
	t.Helper()
	f, err := os.Open("morethanfaangm-audit.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = '\t'
	records, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 484 {
		t.Fatalf("audit has %d data rows, want 483", len(records)-1)
	}
	if got := records[0]; len(got) != 11 || got[0] != "ordinal" || got[10] != "evidence_or_error" {
		t.Fatalf("unexpected audit header: %v", got)
	}
	rows := make([]auditRow, 0, 483)
	for i, record := range records[1:] {
		if len(record) != 11 {
			t.Fatalf("row %d has %d fields, want 11", i+1, len(record))
		}
		if record[0] != strconv.Itoa(i+1) {
			t.Fatalf("row %d has ordinal %q", i+1, record[0])
		}
		rows = append(rows, auditRow{record[0], record[1], record[5], record[6], record[7]})
	}
	return rows
}

func TestAuditAccountsForEveryUpstreamRow(t *testing.T) {
	want := map[string]int{
		"validated_supported": 137,
		"duplicate":           9,
		"unsupported":         261,
		"dead":                36,
		"manual_review":       39,
		"not_a_company":       1,
	}
	got := map[string]int{}
	for _, row := range readAudit(t) {
		got[row.Disposition]++
		if _, ok := want[row.Disposition]; !ok {
			t.Fatalf("ordinal %s has unknown disposition %q", row.Ordinal, row.Disposition)
		}
		if row.ParamsJSON != "" {
			var value any
			if err := json.Unmarshal([]byte(row.ParamsJSON), &value); err != nil {
				t.Fatalf("ordinal %s has invalid params JSON: %v", row.Ordinal, err)
			}
		}
	}
	for disposition, count := range want {
		if got[disposition] != count {
			t.Errorf("%s count = %d, want %d", disposition, got[disposition], count)
		}
	}
}

func TestEveryValidatedBoardIsConfiguredOnce(t *testing.T) {
	cfg, err := config.Load("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	configured := map[string]string{}
	for _, company := range cfg.Companies {
		s, err := source.New(company.Source, company.Name, company.Params, client)
		if err != nil {
			t.Fatalf("configured company %q: %v", company.Name, err)
		}
		id := source.Identity(s)
		if previous, exists := configured[id]; exists {
			t.Fatalf("configured board %q is duplicated by %q and %q", id, previous, company.Name)
		}
		configured[id] = company.Name
	}
	if len(configured) != 186 {
		t.Fatalf("configured source count = %d, want 186", len(configured))
	}

	for _, row := range readAudit(t) {
		if row.Disposition != "validated_supported" {
			continue
		}
		var raw any
		if err := json.Unmarshal([]byte(row.ParamsJSON), &raw); err != nil {
			t.Fatalf("ordinal %s: %v", row.Ordinal, err)
		}
		values := []any{raw}
		if list, ok := raw.([]any); ok {
			values = list
		}
		for _, value := range values {
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("ordinal %s params are not an object", row.Ordinal)
			}
			sourceName := row.Source
			if embedded, ok := object["source"].(string); ok {
				sourceName = embedded
			}
			p := params.Map{}
			for key, value := range object {
				if key != "source" {
					p[key] = fmt.Sprint(value)
				}
			}
			s, err := source.New(sourceName, row.Name, p, client)
			if err != nil {
				t.Fatalf("ordinal %s source construction: %v", row.Ordinal, err)
			}
			if _, ok := configured[source.Identity(s)]; !ok {
				t.Errorf("ordinal %s validated board %q is absent from config", row.Ordinal, source.Identity(s))
			}
		}
	}
}
