package source

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"jobwatch/internal/params"
)

func customTestUUID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

func customTestFlight(t *testing.T, payload string) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return `<script>self.__next_f.push([1,` + string(encoded) + `])</script>`
}

func TestCustomBoardFactoriesAreRegistered(t *testing.T) {
	tests := []struct {
		name     string
		params   params.Map
		wantType string
	}{
		{
			name: "keka",
			params: params.Map{
				"host": "squadrun.keka.com", "portal": "default",
				"identifier": "c750f148-70b8-4a21-868e-f891a1b2d818",
			},
			wantType: "*source.keka",
		},
		{name: "fastenal", wantType: "*source.fastenal"},
		{name: "siemensjobs", wantType: "*source.siemensJobs"},
		{name: "nykaa", wantType: "*source.nykaa"},
		{name: "airoha", wantType: "*source.airoha"},
		{name: "payu", wantType: "*source.payu"},
		{name: "forty2gears", wantType: "*source.forty2Gears"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, err := New(test.name, "Acme", test.params, &http.Client{})
			if err != nil {
				t.Fatal(err)
			}
			if src.Company() != "Acme" {
				t.Errorf("Company() = %q, want Acme", src.Company())
			}
			wrapped, ok := src.(*identifiedSource)
			if !ok {
				t.Fatalf("New returned %T, want *identifiedSource", src)
			}
			if got := fmt.Sprintf("%T", wrapped.Source); got != test.wantType {
				t.Errorf("underlying type = %q, want %q", got, test.wantType)
			}
		})
	}
}
