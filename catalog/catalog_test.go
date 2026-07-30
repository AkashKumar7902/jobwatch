package catalog_test

import (
	"crypto/sha256"
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
	OriginalURL string
	Disposition string
	Source      string
	ParamsJSON  string
	APIURL      string
	Evidence    string
}

type auditSpec struct {
	path                string
	rows                int
	fingerprint         string
	validatedIdentities int
	expected            map[string]int
}

var auditSpecs = []auditSpec{
	{
		path:                "morethanfaangm-audit.tsv",
		rows:                483,
		fingerprint:         "63c3ca7fc91569309e5c930941095f52d54a7421202778f06382e43779801fa5",
		validatedIdentities: 145,
		expected: map[string]int{
			"validated_supported": 143,
			"duplicate":           9,
			"unsupported":         256,
			"dead":                36,
			"manual_review":       38,
			"not_a_company":       1,
		},
	},
	{
		path:                "list-of-companies-audit.tsv",
		rows:                131,
		fingerprint:         "4394e63ad3c23cb83552a5b8478b425095346e8dc3a76196d085bee35b14586f",
		validatedIdentities: 13,
		expected: map[string]int{
			"validated_supported": 13,
			"duplicate":           34,
			"unsupported":         66,
			"dead":                13,
			"manual_review":       5,
		},
	},
}

func readAudit(t *testing.T, path string, wantRows int) []auditRow {
	t.Helper()
	f, err := os.Open(path)
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
	if len(records) != wantRows+1 {
		t.Fatalf("%s has %d data rows, want %d", path, len(records)-1, wantRows)
	}
	if got := records[0]; len(got) != 11 || got[0] != "ordinal" || got[10] != "evidence_or_error" {
		t.Fatalf("%s has unexpected audit header: %v", path, got)
	}
	rows := make([]auditRow, 0, wantRows)
	for i, record := range records[1:] {
		if len(record) != 11 {
			t.Fatalf("%s row %d has %d fields, want 11", path, i+1, len(record))
		}
		if record[0] != strconv.Itoa(i+1) {
			t.Fatalf("%s row %d has ordinal %q", path, i+1, record[0])
		}
		rows = append(rows, auditRow{
			Ordinal:     record[0],
			Name:        record[1],
			OriginalURL: record[2],
			Disposition: record[5],
			Source:      record[6],
			ParamsJSON:  record[7],
			APIURL:      record[8],
			Evidence:    record[10],
		})
	}
	return rows
}

func auditFingerprint(rows []auditRow) string {
	hash := sha256.New()
	for _, row := range rows {
		_, _ = fmt.Fprintf(hash, "%s\t%s\t%s\n", row.Ordinal, row.Name, row.OriginalURL)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func auditRowIdentities(t *testing.T, row auditRow, client *http.Client) []string {
	t.Helper()
	var raw any
	if err := json.Unmarshal([]byte(row.ParamsJSON), &raw); err != nil {
		t.Fatalf("ordinal %s has invalid params JSON: %v", row.Ordinal, err)
	}
	values := []any{raw}
	if list, ok := raw.([]any); ok {
		values = list
	}
	identities := make([]string, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("ordinal %s params are not an object", row.Ordinal)
		}
		sourceName := row.Source
		if embedded, ok := object["source"].(string); ok {
			sourceName = embedded
		}
		if sourceName == "" {
			t.Fatalf("ordinal %s has params but no source", row.Ordinal)
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
		identities = append(identities, source.Identity(s))
	}
	return identities
}

func TestAuditsAccountForEveryUpstreamRow(t *testing.T) {
	for _, audit := range auditSpecs {
		t.Run(audit.path, func(t *testing.T) {
			rows := readAudit(t, audit.path, audit.rows)
			if got := auditFingerprint(rows); got != audit.fingerprint {
				t.Fatalf("ordered provenance fingerprint = %s, want %s", got, audit.fingerprint)
			}
			got := map[string]int{}
			for _, row := range rows {
				got[row.Disposition]++
				if _, ok := audit.expected[row.Disposition]; !ok {
					t.Fatalf("ordinal %s has unknown disposition %q", row.Ordinal, row.Disposition)
				}
				if row.Evidence == "" {
					t.Fatalf("ordinal %s has no evidence", row.Ordinal)
				}
				if row.ParamsJSON != "" {
					var value any
					if err := json.Unmarshal([]byte(row.ParamsJSON), &value); err != nil {
						t.Fatalf("ordinal %s has invalid params JSON: %v", row.Ordinal, err)
					}
				}
				if row.Disposition == "validated_supported" {
					if row.Source == "" || row.ParamsJSON == "" || row.APIURL == "" {
						t.Fatalf("ordinal %s validated row lacks source, params, or API URL", row.Ordinal)
					}
				}
			}
			for disposition, count := range audit.expected {
				if got[disposition] != count {
					t.Errorf("%s count = %d, want %d", disposition, got[disposition], count)
				}
			}
		})
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
	if len(configured) != 204 {
		t.Fatalf("configured source count = %d, want 204", len(configured))
	}

	validatedSets := map[string]map[string]struct{}{}
	for _, audit := range auditSpecs {
		validated := map[string]struct{}{}
		for _, row := range readAudit(t, audit.path, audit.rows) {
			if row.Disposition != "validated_supported" && !(row.Disposition == "duplicate" && row.ParamsJSON != "") {
				continue
			}
			for _, identity := range auditRowIdentities(t, row, client) {
				if _, ok := configured[identity]; !ok {
					t.Errorf("%s ordinal %s %s board %q is absent from config", audit.path, row.Ordinal, row.Disposition, identity)
				}
				if row.Disposition == "validated_supported" {
					validated[identity] = struct{}{}
				}
			}
		}
		if len(validated) != audit.validatedIdentities {
			t.Errorf("%s validated identity count = %d, want %d", audit.path, len(validated), audit.validatedIdentities)
		}
		validatedSets[audit.path] = validated
	}

	legacy := validatedSets["morethanfaangm-audit.tsv"]
	listOfCompanies := validatedSets["list-of-companies-audit.tsv"]
	union := map[string]struct{}{}
	for identity := range legacy {
		union[identity] = struct{}{}
	}
	overlap := 0
	for identity := range listOfCompanies {
		if _, ok := union[identity]; ok {
			overlap++
		}
		union[identity] = struct{}{}
	}
	if overlap != 5 {
		t.Errorf("validated identity overlap = %d, want 5", overlap)
	}
	if len(union) != 153 {
		t.Errorf("validated identity union = %d, want 153", len(union))
	}
	configuredOutsideAudits := 0
	for identity := range configured {
		if _, ok := union[identity]; !ok {
			configuredOutsideAudits++
		}
	}
	if configuredOutsideAudits != 51 {
		t.Errorf("configured identities outside audits = %d, want 51", configuredOutsideAudits)
	}
}
