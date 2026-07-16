// Package model holds the data types shared by every part of jobwatch.
package model

import "time"

// Job is the normalized representation of a job posting, regardless of
// which ATS (applicant tracking system) it came from. Sources are
// responsible for converting their API's shape into this one.
type Job struct {
	// ID uniquely identifies a posting across runs. Sources build it as
	// "<source>/<board>/<posting-id>" so it stays stable between polls
	// and never collides across companies.
	ID string

	Company  string
	Title    string
	Location string
	URL      string

	// EmploymentType is the ATS's own label ("Full-time", "FullTime",
	// "fulltime_permanent", "Contract", ...). Empty when the ATS doesn't
	// expose one (Greenhouse never does).
	EmploymentType string

	// Description is the full posting text with HTML stripped.
	// Matchers run against Title + Description.
	Description string

	// PostedAt is zero when the ATS doesn't expose a date.
	PostedAt time.Time
}
