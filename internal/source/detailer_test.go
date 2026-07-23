package source

import (
	"context"
	"net/http"
	"testing"

	"jobwatch/internal/model"
)

// Regression: New() wraps sources in identifiedSource, which must forward
// the optional Detailer interface — otherwise lazy-detail boards evaluate
// jobs with empty descriptions.
func TestWrapperForwardsDetailer(t *testing.T) {
	// workday is a Detailer; a wrapped instance must still assert as one.
	s, err := New("workday", "Acme", map[string]string{
		"host": "x.wd1.myworkdayjobs.com", "tenant": "x", "site": "External",
	}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(Detailer); !ok {
		t.Fatal("wrapped workday source does not expose Detailer")
	}

	// greenhouse is not a Detailer; the wrapper's Detail must no-op safely.
	g, err := New("greenhouse", "Acme", map[string]string{"board_token": "acme"}, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := g.(Detailer)
	if !ok {
		t.Fatal("wrapper should always expose Detail")
	}
	job := model.Job{Description: "kept"}
	if err := d.Detail(context.Background(), &job); err != nil {
		t.Errorf("non-detailer wrapper Detail should no-op, got %v", err)
	}
	if job.Description != "kept" {
		t.Error("no-op Detail must not mutate the job")
	}
}
