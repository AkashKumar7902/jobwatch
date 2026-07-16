package match

// The "recency" matcher passes only postings published within the last N
// days — useful against evergreen postings that sit open for months.
// ATSes that don't expose a date (Recruitee, BambooHR, and some Workday
// boards) yield jobs with an unknown date; match_when_unknown controls
// whether those pass (default true, so no jobs are silently dropped).
//
//	matcher:
//	  name: recency
//	  params:
//	    max_days: 30
//	    match_when_unknown: true

import (
	"fmt"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

func init() {
	Register("recency", func(p params.Map, children []Matcher) (Matcher, error) {
		if err := RequireNoChildren("recency", children); err != nil {
			return nil, err
		}
		maxDays, err := p.Int("max_days", 0)
		if err != nil {
			return nil, err
		}
		if maxDays <= 0 {
			return nil, fmt.Errorf(`needs "max_days" (a positive number of days)`)
		}
		unknown, err := p.Bool("match_when_unknown", true)
		if err != nil {
			return nil, err
		}
		return &recency{maxAge: time.Duration(maxDays) * 24 * time.Hour, matchUnknown: unknown}, nil
	})
}

type recency struct {
	maxAge       time.Duration
	matchUnknown bool
}

func (r *recency) Name() string { return "recency" }

func (r *recency) Match(job model.Job) Result {
	if job.PostedAt.IsZero() {
		return Result{Matched: r.matchUnknown, Reason: "posting date unknown"}
	}
	age := time.Since(job.PostedAt)
	days := int(age.Hours() / 24)
	if age <= r.maxAge {
		return Result{Matched: true, Reason: fmt.Sprintf("posted %d day(s) ago", days)}
	}
	return Result{Matched: false, Reason: fmt.Sprintf("posted %d day(s) ago, older than %d days", days, int(r.maxAge.Hours()/24))}
}
