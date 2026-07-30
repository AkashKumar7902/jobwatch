package source

// MyNextHire public careers API (no authentication). The active-requisition
// endpoint returns complete descriptions, so one request lists the whole
// board.
//
// Config:
//
//	- name: ShareChat
//	  source: mynexthire
//	  params:
//	    host: sharechat.mynexthire.com

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const myNextHireActiveStatusID = 3

func init() {
	Register("mynexthire", func(company string, p params.Map, client *http.Client) (Source, error) {
		rawHost, err := p.Require("host")
		if err != nil {
			return nil, err
		}
		host, err := normalizeBoardHost(rawHost)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", "host", err)
		}
		return &myNextHire{
			company: company, host: host, baseURL: "https://" + host, client: client,
		}, nil
	})
}

type myNextHire struct {
	company string
	host    string
	baseURL string
	client  *http.Client
}

func (s *myNextHire) Company() string { return s.company }

type myNextHirePosting struct {
	ReqID           int64  `json:"reqId"`
	StatusID        int    `json:"statusId"`
	ReqTitle        string `json:"reqTitle"`
	JDDisplay       string `json:"jdDisplay"`
	Location        string `json:"location"`
	LocationAddress string `json:"locationAddress"`
	EmploymentType  string `json:"employmentType"`
	ApprovedOn      string `json:"approvedOn"`
	LocationList    []struct {
		Office  string `json:"office"`
		Address string `json:"address"`
	} `json:"locationList"`
}

func (s *myNextHire) Fetch(ctx context.Context) ([]model.Job, error) {
	var response struct {
		ReqDetails *[]myNextHirePosting `json:"reqDetailsBOList"`
	}
	body := []byte(`{"source":"careers","code":"","filterByBuId":-1}`)
	endpoint := s.baseURL + "/employer/careers/reqlist/get"
	if err := fetchJSON(ctx, s.client, http.MethodPost, endpoint, body, &response); err != nil {
		return nil, err
	}
	if response.ReqDetails == nil {
		return nil, fmt.Errorf("mynexthire %s omitted reqDetailsBOList", s.host)
	}

	postings := *response.ReqDetails
	jobs := make([]model.Job, 0, len(postings))
	seen := make(map[int64]struct{}, len(postings))
	for i, posting := range postings {
		if posting.ReqID <= 0 {
			return nil, fmt.Errorf("mynexthire %s: item %d has invalid reqId %d", s.host, i, posting.ReqID)
		}
		if _, duplicate := seen[posting.ReqID]; duplicate {
			return nil, fmt.Errorf("mynexthire %s: duplicate reqId %d", s.host, posting.ReqID)
		}
		seen[posting.ReqID] = struct{}{}
		if posting.StatusID != myNextHireActiveStatusID {
			return nil, fmt.Errorf(
				"mynexthire %s: item %d (%d) has statusId %d, want %d",
				s.host, i, posting.ReqID, posting.StatusID, myNextHireActiveStatusID,
			)
		}
		title := strings.TrimSpace(posting.ReqTitle)
		if title == "" {
			return nil, fmt.Errorf("mynexthire %s: item %d has an empty reqTitle", s.host, i)
		}
		description := strings.TrimSpace(posting.JDDisplay)
		if description == "" {
			return nil, fmt.Errorf("mynexthire %s: item %d (%d) has an empty jdDisplay", s.host, i, posting.ReqID)
		}
		postedAt, err := parseMyNextHireTime(posting.ApprovedOn)
		if err != nil {
			return nil, fmt.Errorf(
				"mynexthire %s: item %d (%d) has invalid approvedOn: %w",
				s.host, i, posting.ReqID, err,
			)
		}

		var locations []string
		for _, location := range posting.LocationList {
			if office := strings.TrimSpace(location.Office); office != "" {
				locations = append(locations, office)
			} else if address := strings.TrimSpace(location.Address); address != "" {
				locations = append(locations, address)
			}
		}
		locations = distinctStrings(locations)
		if len(locations) == 0 {
			locations = distinctStrings([]string{posting.Location, posting.LocationAddress})
		}

		jobs = append(jobs, model.Job{
			ID:             fmt.Sprintf("mynexthire/%s/%d", s.host, posting.ReqID),
			Company:        s.company,
			Title:          title,
			Location:       strings.Join(locations, "; "),
			URL:            myNextHireJobURL(s.host, posting.ReqID),
			EmploymentType: strings.TrimSpace(posting.EmploymentType),
			Description:    description,
			PostedAt:       postedAt,
		})
	}
	return jobs, nil
}

func myNextHireJobURL(host string, reqID int64) string {
	payload, _ := json.Marshal(map[string]any{
		"pageType": "jd",
		"cvSource": "careers",
		"reqId":    reqID,
		"requester": map[string]string{
			"id": "", "code": "", "name": "",
		},
		"page":         "careers",
		"bufilter":     -1,
		"customFields": map[string]any{},
	})
	return "https://" + host + "/employer/jobs?src=careers&p=" +
		url.QueryEscape(base64.StdEncoding.EncodeToString(payload))
}

func parseMyNextHireTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-0700",
		time.RFC3339Nano,
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}
