package source

// OfBusiness publishes its complete Jobs collection in the server-rendered
// Wix warmup payload on the public careers page. Wix serves four collection
// records per page and declares both the collection total and page count.
// Each record already contains the full posting, so no detail requests are
// needed.
//
//	GET https://www.ofbcareers.com/categories
//	GET https://www.ofbcareers.com/categories?<wix-dataset>_page={page}

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jobwatch/internal/htmltext"
	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

const (
	ofBusinessSiteBase         = "https://www.ofbcareers.com"
	ofBusinessBodyLimit        = 4 << 20
	ofBusinessMaxJobs          = 1_000
	ofBusinessMaxPages         = 250
	ofBusinessMaxPage          = 100
	ofBusinessPlaceholderID    = "088aba40-d05e-4272-b075-c3a3c55e55fd"
	ofBusinessPlaceholderOwner = "525fa184-f33d-4c0b-a3a7-783be3a8de5a"
	ofBusinessPlaceholderDate  = "2025-07-31T13:46:00.534Z"
)

var ofBusinessTitleRE = regexp.MustCompile(`(?is)<title(?:\s[^>]*)?>(.*?)</title>`)

func init() {
	Register("ofbusiness", func(company string, p params.Map, client *http.Client) (Source, error) {
		if len(p) != 0 {
			keys := make([]string, 0, len(p))
			for key := range p {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("ofbusiness source does not accept params (got %s)", strings.Join(keys, ", "))
		}
		if client == nil {
			client = http.DefaultClient
		}
		return &ofBusiness{
			company: company,
			base:    ofBusinessSiteBase,
			client:  client,
		}, nil
	})
}

type ofBusiness struct {
	company string
	base    string
	client  *http.Client
}

type ofBusinessWarmup struct {
	Platform       *ofBusinessPlatform       `json:"platform"`
	AppsWarmupData *ofBusinessAppsWarmupData `json:"appsWarmupData"`
}

type ofBusinessPlatform struct {
	SSRPropsUpdates *[]map[string]json.RawMessage `json:"ssrPropsUpdates"`
}

type ofBusinessAppsWarmupData struct {
	DataBinding *ofBusinessDataBinding `json:"dataBinding"`
}

type ofBusinessDataBinding struct {
	Schemas   map[string]json.RawMessage `json:"schemas"`
	DataStore *ofBusinessDataStore       `json:"dataStore"`
}

type ofBusinessDataStore struct {
	RecordInfosByDatasetID map[string]ofBusinessRecordInfo       `json:"recordInfosByDatasetId"`
	RecordsByCollectionID  map[string]map[string]json.RawMessage `json:"recordsByCollectionId"`
}

type ofBusinessRecordInfo struct {
	ItemIDs     *[]string              `json:"itemIds"`
	DatasetSize *ofBusinessDatasetSize `json:"datasetSize"`
}

type ofBusinessDatasetSize struct {
	Total  *int `json:"total"`
	Loaded *int `json:"loaded"`
}

type ofBusinessComponentState struct {
	Items       *[]string `json:"items"`
	CurrentPage *int      `json:"currentPage"`
	TotalPages  *int      `json:"totalPages"`
}

type ofBusinessDate struct {
	Date string `json:"$date"`
}

type ofBusinessCategory struct {
	ID       string `json:"_id"`
	Category string `json:"category"`
}

type ofBusinessPosting struct {
	ID                      string              `json:"_id"`
	Owner                   string              `json:"_owner"`
	CreatedDate             *ofBusinessDate     `json:"_createdDate"`
	UpdatedDate             *ofBusinessDate     `json:"_updatedDate"`
	EmpID                   string              `json:"empId"`
	JobCode                 int                 `json:"jobCode"`
	AllJobsPath             string              `json:"link-jobs-1-all"`
	JobTitle                string              `json:"jobTitle"`
	Location                string              `json:"location"`
	JobType                 string              `json:"jobType"`
	ExperienceRequiredRange string              `json:"experienceRequiredRangeyrs"`
	JobDescription          string              `json:"jobDescription"`
	WhatYouWillDo           string              `json:"whatYouWillDo"`
	WhatWeAreLookingFor     string              `json:"whatWeAreLookingFor"`
	WhatWeAreOffering       string              `json:"whatWeAreOffering"`
	DetailPath              string              `json:"link-jobs-jobTitle"`
	Category                *ofBusinessCategory `json:"category"`
}

type ofBusinessPage struct {
	currentPage int
	totalPages  int
	total       int
	loaded      int
	itemIDs     []string
	records     map[string]json.RawMessage
	document    string
}

func (s *ofBusiness) Company() string { return s.company }

func (s *ofBusiness) Fetch(ctx context.Context) ([]model.Job, error) {
	categoryURL, err := ofBusinessCategoryURL(s.base)
	if err != nil {
		return nil, fmt.Errorf("ofbusiness: invalid site base: %w", err)
	}
	endpoint := categoryURL.String()

	var (
		jobs          []model.Job
		seen          = make(map[string]struct{})
		expectedTotal = -1
		pageSize      = -1
		totalPages    = -1
		pageQueryKey  string
		placeholders  int
		postingsByID  = make(map[string]ofBusinessPosting)
	)
	for pageNumber := 1; ; pageNumber++ {
		body, err := s.fetchHTML(ctx, endpoint, categoryURL)
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: page %d: %w", pageNumber, err)
		}
		page, err := parseOfBusinessPage(body)
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: page %d: %w", pageNumber, err)
		}
		if page.currentPage != pageNumber {
			return nil, fmt.Errorf(
				"ofbusiness: page %d declared currentPage %d",
				pageNumber, page.currentPage,
			)
		}

		if pageNumber == 1 {
			expectedTotal = page.total
			pageSize = page.loaded
			totalPages = page.totalPages
			if expectedTotal < 0 || expectedTotal > ofBusinessMaxJobs {
				return nil, fmt.Errorf(
					"ofbusiness: collection total %d is outside safety limit 0..%d",
					expectedTotal, ofBusinessMaxJobs,
				)
			}
			if totalPages <= 0 || totalPages > ofBusinessMaxPages {
				return nil, fmt.Errorf(
					"ofbusiness: totalPages %d is outside safety limit 1..%d",
					totalPages, ofBusinessMaxPages,
				)
			}
			if expectedTotal == 0 {
				if pageSize != 0 || totalPages != 1 {
					return nil, fmt.Errorf(
						"ofbusiness: empty collection declared loaded=%d totalPages=%d",
						pageSize, totalPages,
					)
				}
			} else {
				if pageSize <= 0 || pageSize > ofBusinessMaxPage {
					return nil, fmt.Errorf(
						"ofbusiness: page size %d is outside safety limit 1..%d",
						pageSize, ofBusinessMaxPage,
					)
				}
				wantPages := (expectedTotal + pageSize - 1) / pageSize
				if totalPages != wantPages {
					return nil, fmt.Errorf(
						"ofbusiness: total=%d loaded=%d implies %d pages, site declared %d",
						expectedTotal, pageSize, wantPages, totalPages,
					)
				}
			}
		} else {
			if page.total != expectedTotal {
				return nil, fmt.Errorf(
					"ofbusiness: collection total changed from %d to %d on page %d",
					expectedTotal, page.total, pageNumber,
				)
			}
			if page.totalPages != totalPages {
				return nil, fmt.Errorf(
					"ofbusiness: totalPages changed from %d to %d on page %d",
					totalPages, page.totalPages, pageNumber,
				)
			}
		}

		wantLoaded := 0
		if expectedTotal > 0 {
			remaining := expectedTotal - (pageNumber-1)*pageSize
			if remaining > pageSize {
				wantLoaded = pageSize
			} else {
				wantLoaded = remaining
			}
		}
		if wantLoaded < 0 || page.loaded != wantLoaded || len(page.itemIDs) != wantLoaded {
			return nil, fmt.Errorf(
				"ofbusiness: page %d loaded %d records (%d item IDs), want %d",
				pageNumber, page.loaded, len(page.itemIDs), wantLoaded,
			)
		}

		for index, recordID := range page.itemIDs {
			if !customBoardUUIDRE.MatchString(recordID) {
				return nil, fmt.Errorf(
					"ofbusiness: page %d item %d has invalid record ID %q",
					pageNumber, index, recordID,
				)
			}
			if _, duplicate := seen[recordID]; duplicate {
				return nil, fmt.Errorf("ofbusiness: duplicate record ID %q", recordID)
			}
			raw, ok := page.records[recordID]
			if !ok {
				return nil, fmt.Errorf(
					"ofbusiness: page %d item %d record %q is missing",
					pageNumber, index, recordID,
				)
			}
			var posting ofBusinessPosting
			if err := json.Unmarshal(raw, &posting); err != nil {
				return nil, fmt.Errorf(
					"ofbusiness: page %d item %d record %q: decoding: %w",
					pageNumber, index, recordID, err,
				)
			}
			if posting.ID != recordID {
				return nil, fmt.Errorf(
					"ofbusiness: page %d item %d map ID %q does not match record ID %q",
					pageNumber, index, recordID, posting.ID,
				)
			}
			seen[recordID] = struct{}{}

			if posting.isPlaceholder() {
				if err := validateOfBusinessPlaceholder(posting); err != nil {
					return nil, fmt.Errorf(
						"ofbusiness: page %d item %d placeholder %q: %w",
						pageNumber, index, recordID, err,
					)
				}
				placeholders++
				if placeholders > 1 {
					return nil, fmt.Errorf("ofbusiness: collection contains more than one empty placeholder")
				}
				continue
			}
			job, err := s.normalizePosting(posting)
			if err != nil {
				return nil, fmt.Errorf(
					"ofbusiness: page %d item %d record %q: %w",
					pageNumber, index, recordID, err,
				)
			}
			postingsByID[job.ID] = posting
			jobs = append(jobs, job)
		}

		prev, hasPrev, err := ofBusinessPageLink(page.document, "prev")
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: page %d: %w", pageNumber, err)
		}
		next, hasNext, err := ofBusinessPageLink(page.document, "next")
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: page %d: %w", pageNumber, err)
		}
		if pageNumber == 1 {
			if hasPrev {
				return nil, fmt.Errorf("ofbusiness: first page unexpectedly declares a prev link")
			}
		} else {
			if !hasPrev {
				return nil, fmt.Errorf("ofbusiness: page %d omitted its prev link", pageNumber)
			}
			if _, err := validateOfBusinessPageURL(
				categoryURL, prev, pageQueryKey, pageNumber-1,
			); err != nil {
				return nil, fmt.Errorf("ofbusiness: page %d prev link: %w", pageNumber, err)
			}
		}

		if pageNumber == totalPages {
			if hasNext {
				return nil, fmt.Errorf("ofbusiness: final page %d unexpectedly declares a next link", pageNumber)
			}
			break
		}
		if !hasNext {
			return nil, fmt.Errorf("ofbusiness: page %d omitted its next link", pageNumber)
		}
		nextURL, err := validateOfBusinessPageURL(
			categoryURL, next, pageQueryKey, pageNumber+1,
		)
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: page %d next link: %w", pageNumber, err)
		}
		if pageQueryKey == "" {
			for key := range nextURL.Query() {
				pageQueryKey = key
			}
		}
		endpoint = nextURL.String()
	}

	if len(seen) != expectedTotal {
		return nil, fmt.Errorf(
			"ofbusiness: collected %d unique collection records, want %d",
			len(seen), expectedTotal,
		)
	}
	actionable, err := s.resolveDuplicateDetailURLs(ctx, jobs, postingsByID)
	if err != nil {
		return nil, err
	}
	return actionable, nil
}

func (p ofBusinessPosting) isPlaceholder() bool {
	return strings.TrimSpace(p.EmpID) == "" &&
		strings.TrimSpace(p.JobTitle) == "" &&
		strings.TrimSpace(p.Location) == "" &&
		strings.TrimSpace(p.JobType) == "" &&
		strings.TrimSpace(p.ExperienceRequiredRange) == "" &&
		strings.TrimSpace(p.JobDescription) == "" &&
		strings.TrimSpace(p.WhatYouWillDo) == "" &&
		strings.TrimSpace(p.WhatWeAreLookingFor) == "" &&
		strings.TrimSpace(p.WhatWeAreOffering) == "" &&
		strings.TrimSpace(p.DetailPath) == "" &&
		p.Category == nil
}

func validateOfBusinessPlaceholder(posting ofBusinessPosting) error {
	if posting.ID != ofBusinessPlaceholderID ||
		posting.Owner != ofBusinessPlaceholderOwner ||
		posting.JobCode != 202401 ||
		posting.AllJobsPath != "/jobs-1/" {
		return fmt.Errorf("does not match the one validated CMS shell")
	}
	createdAt, err := parseOfBusinessDate(posting.CreatedDate)
	if err != nil {
		return fmt.Errorf("invalid _createdDate: %w", err)
	}
	updatedAt, err := parseOfBusinessDate(posting.UpdatedDate)
	if err != nil {
		return fmt.Errorf("invalid _updatedDate: %w", err)
	}
	if posting.CreatedDate.Date != ofBusinessPlaceholderDate ||
		posting.UpdatedDate.Date != ofBusinessPlaceholderDate ||
		!createdAt.Equal(updatedAt) {
		return fmt.Errorf(
			"timestamps %s/%s do not match the validated CMS shell",
			createdAt.Format(time.RFC3339Nano), updatedAt.Format(time.RFC3339Nano),
		)
	}
	return nil
}

type ofBusinessDetailIdentity struct {
	title      string
	location   string
	experience string
}

func (s *ofBusiness) resolveDuplicateDetailURLs(
	ctx context.Context,
	jobs []model.Job,
	postingsByID map[string]ofBusinessPosting,
) ([]model.Job, error) {
	indexesByURL := make(map[string][]int)
	for index, job := range jobs {
		indexesByURL[job.URL] = append(indexesByURL[job.URL], index)
	}
	keep := make([]bool, len(jobs))
	for index := range keep {
		keep[index] = true
	}
	for detailURL, indexes := range indexesByURL {
		if len(indexes) == 1 {
			continue
		}
		identity, err := s.fetchDetailIdentity(ctx, detailURL)
		if err != nil {
			return nil, fmt.Errorf("ofbusiness: resolving duplicate detail URL %s: %w", detailURL, err)
		}
		matched := -1
		for _, index := range indexes {
			posting, ok := postingsByID[jobs[index].ID]
			if !ok {
				return nil, fmt.Errorf("ofbusiness: missing posting state for %q", jobs[index].ID)
			}
			if strings.EqualFold(compactSpaces(posting.JobTitle), identity.title) &&
				strings.EqualFold(compactSpaces(posting.Location), identity.location) &&
				compactSpaces(posting.ExperienceRequiredRange) == identity.experience {
				if matched >= 0 {
					return nil, fmt.Errorf(
						"ofbusiness: duplicate detail URL %s matches more than one collection record",
						detailURL,
					)
				}
				matched = index
			}
		}
		if matched < 0 {
			return nil, fmt.Errorf(
				"ofbusiness: duplicate detail URL %s matches none of its %d collection records",
				detailURL, len(indexes),
			)
		}
		for _, index := range indexes {
			keep[index] = index == matched
		}
	}
	actionable := make([]model.Job, 0, len(jobs))
	for index, job := range jobs {
		if keep[index] {
			actionable = append(actionable, job)
		}
	}
	return actionable, nil
}

func (s *ofBusiness) fetchDetailIdentity(
	ctx context.Context,
	endpoint string,
) (ofBusinessDetailIdentity, error) {
	expected, err := url.Parse(endpoint)
	if err != nil {
		return ofBusinessDetailIdentity{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ofBusinessDetailIdentity{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := clientWithoutRedirects(client).Do(req)
	if err != nil {
		return ofBusinessDetailIdentity{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ofBusinessDetailIdentity{}, fmt.Errorf("GET %s: %s", endpoint, response.Status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return ofBusinessDetailIdentity{}, fmt.Errorf(
			"GET %s: unexpected Content-Type %q", endpoint, response.Header.Get("Content-Type"),
		)
	}
	if response.Request == nil || response.Request.URL == nil ||
		response.Request.URL.User != nil ||
		response.Request.URL.Scheme != expected.Scheme ||
		!strings.EqualFold(response.Request.URL.Host, expected.Host) ||
		response.Request.URL.EscapedPath() != expected.EscapedPath() ||
		response.Request.URL.RawQuery != "" || response.Request.URL.ForceQuery ||
		response.Request.URL.Fragment != "" {
		return ofBusinessDetailIdentity{}, fmt.Errorf("GET %s: redirected away from the canonical job page", endpoint)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ofBusinessBodyLimit+1))
	if err != nil {
		return ofBusinessDetailIdentity{}, err
	}
	if len(body) > ofBusinessBodyLimit {
		return ofBusinessDetailIdentity{}, fmt.Errorf("GET %s: response exceeds safety limit", endpoint)
	}
	matches := ofBusinessTitleRE.FindAllStringSubmatch(string(body), -1)
	if len(matches) != 1 {
		return ofBusinessDetailIdentity{}, fmt.Errorf("expected one title element, found %d", len(matches))
	}
	titleParts := strings.Split(cleanHTMLFragment(matches[0][1]), "||")
	if len(titleParts) != 3 {
		return ofBusinessDetailIdentity{}, fmt.Errorf("job-page title does not contain title, location, and experience")
	}
	identity := ofBusinessDetailIdentity{
		title:      compactSpaces(titleParts[0]),
		location:   compactSpaces(titleParts[1]),
		experience: compactSpaces(titleParts[2]),
	}
	if identity.title == "" || identity.location == "" || identity.experience == "" {
		return ofBusinessDetailIdentity{}, fmt.Errorf("job-page title contains an empty identity field")
	}
	return identity, nil
}

func (s *ofBusiness) normalizePosting(posting ofBusinessPosting) (model.Job, error) {
	if strings.TrimSpace(posting.EmpID) == "" {
		return model.Job{}, fmt.Errorf("empty empId")
	}
	title := strings.TrimSpace(posting.JobTitle)
	if title == "" {
		return model.Job{}, fmt.Errorf("empty jobTitle")
	}
	location := strings.TrimSpace(posting.Location)
	if location == "" {
		return model.Job{}, fmt.Errorf("empty location")
	}
	employmentType := strings.TrimSpace(posting.JobType)
	if employmentType == "" {
		return model.Job{}, fmt.Errorf("empty jobType")
	}
	experience := strings.TrimSpace(posting.ExperienceRequiredRange)
	if experience == "" {
		return model.Job{}, fmt.Errorf("empty experienceRequiredRangeyrs")
	}
	if posting.Category == nil ||
		!customBoardUUIDRE.MatchString(posting.Category.ID) ||
		strings.TrimSpace(posting.Category.Category) == "" {
		return model.Job{}, fmt.Errorf("missing or invalid category")
	}
	category := strings.TrimSpace(posting.Category.Category)

	sections := []struct {
		heading string
		html    string
	}{
		{"About the Business", posting.JobDescription},
		{"What You Will Do", posting.WhatYouWillDo},
		{"What We Are Looking For", posting.WhatWeAreLookingFor},
		{"What We Are Offering", posting.WhatWeAreOffering},
	}
	descriptionParts := []string{
		"Department: " + category,
		"Experience Required: " + experience,
	}
	for _, section := range sections {
		text := htmltext.ToText(section.html)
		if text == "" {
			return model.Job{}, fmt.Errorf("empty %s", section.heading)
		}
		descriptionParts = append(descriptionParts, section.heading+"\n"+text)
	}

	createdAt, err := parseOfBusinessDate(posting.CreatedDate)
	if err != nil {
		return model.Job{}, fmt.Errorf("invalid _createdDate: %w", err)
	}
	updatedAt, err := parseOfBusinessDate(posting.UpdatedDate)
	if err != nil {
		return model.Job{}, fmt.Errorf("invalid _updatedDate: %w", err)
	}
	if updatedAt.Before(createdAt) {
		return model.Job{}, fmt.Errorf("_updatedDate is before _createdDate")
	}

	detailURL, err := resolveOfBusinessDetailURL(s.base, posting.DetailPath)
	if err != nil {
		return model.Job{}, err
	}
	return model.Job{
		ID:             "ofbusiness/" + posting.ID,
		Company:        s.company,
		Title:          title,
		Location:       location,
		URL:            detailURL,
		EmploymentType: employmentType,
		Description:    strings.Join(descriptionParts, "\n\n"),
		PostedAt:       createdAt,
	}, nil
}

func parseOfBusinessDate(value *ofBusinessDate) (time.Time, error) {
	if value == nil || strings.TrimSpace(value.Date) == "" || value.Date != strings.TrimSpace(value.Date) {
		return time.Time{}, fmt.Errorf("missing $date")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.Date)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func parseOfBusinessPage(body []byte) (ofBusinessPage, error) {
	payload, err := ofBusinessWarmupPayload(body)
	if err != nil {
		return ofBusinessPage{}, err
	}
	var warmup ofBusinessWarmup
	if err := json.Unmarshal(payload, &warmup); err != nil {
		return ofBusinessPage{}, fmt.Errorf("decoding wix-warmup-data: %w", err)
	}
	if warmup.Platform == nil || warmup.Platform.SSRPropsUpdates == nil ||
		warmup.AppsWarmupData == nil || warmup.AppsWarmupData.DataBinding == nil ||
		warmup.AppsWarmupData.DataBinding.DataStore == nil {
		return ofBusinessPage{}, fmt.Errorf("wix-warmup-data omitted platform or dataBinding state")
	}
	binding := warmup.AppsWarmupData.DataBinding
	if schema, ok := binding.Schemas["Jobs"]; !ok || len(bytes.TrimSpace(schema)) == 0 ||
		bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
		return ofBusinessPage{}, fmt.Errorf("wix-warmup-data omitted the Jobs schema")
	}
	records, ok := binding.DataStore.RecordsByCollectionID["Jobs"]
	if !ok || records == nil {
		return ofBusinessPage{}, fmt.Errorf("wix-warmup-data omitted the Jobs records collection")
	}
	updates := *warmup.Platform.SSRPropsUpdates
	itemIDs, currentPage, totalPages, err := ofBusinessComponentMetadata(updates, records)
	if err != nil {
		return ofBusinessPage{}, err
	}
	total, loaded, err := ofBusinessDatasetMetadata(
		binding.DataStore.RecordInfosByDatasetID, itemIDs,
	)
	if err != nil {
		return ofBusinessPage{}, err
	}
	return ofBusinessPage{
		currentPage: currentPage,
		totalPages:  totalPages,
		total:       total,
		loaded:      loaded,
		itemIDs:     itemIDs,
		records:     records,
		document:    string(body),
	}, nil
}

func ofBusinessWarmupPayload(body []byte) ([]byte, error) {
	var payload []byte
	for _, match := range jsonLDScript.FindAllStringSubmatch(string(body), -1) {
		attrs := parseHTMLAttrs(match[1])
		if attrs["id"] != "wix-warmup-data" {
			continue
		}
		if attrs["type"] != "application/json" {
			return nil, fmt.Errorf(
				"wix-warmup-data has unexpected type %q",
				attrs["type"],
			)
		}
		if payload != nil {
			return nil, fmt.Errorf("document contains duplicate wix-warmup-data scripts")
		}
		payload = []byte(match[2])
	}
	if payload == nil {
		return nil, fmt.Errorf("document omitted wix-warmup-data")
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, fmt.Errorf("wix-warmup-data is empty")
	}
	return payload, nil
}

func ofBusinessComponentMetadata(
	updates []map[string]json.RawMessage,
	records map[string]json.RawMessage,
) ([]string, int, int, error) {
	var (
		itemCandidates [][]string
		pageCandidates [][2]int
	)
	for updateIndex, update := range updates {
		for componentID, raw := range update {
			var state ofBusinessComponentState
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, 0, 0, fmt.Errorf(
					"component update %d %q: decoding: %w",
					updateIndex, componentID, err,
				)
			}
			if state.CurrentPage != nil || state.TotalPages != nil {
				if state.CurrentPage == nil || state.TotalPages == nil {
					return nil, 0, 0, fmt.Errorf(
						"component %q omitted currentPage or totalPages",
						componentID,
					)
				}
				pair := [2]int{*state.CurrentPage, *state.TotalPages}
				if !containsOfBusinessPagePair(pageCandidates, pair) {
					pageCandidates = append(pageCandidates, pair)
				}
			}
			if state.Items == nil {
				continue
			}
			items := *state.Items
			allJobs := len(items) == 0 && len(records) == 0
			if len(items) > 0 {
				allJobs = true
				for _, id := range items {
					if _, ok := records[id]; !ok {
						allJobs = false
						break
					}
				}
			}
			if allJobs && !containsOfBusinessIDs(itemCandidates, items) {
				itemCandidates = append(itemCandidates, append([]string(nil), items...))
			}
		}
	}
	if len(itemCandidates) != 1 {
		return nil, 0, 0, fmt.Errorf(
			"found %d distinct displayed Jobs item lists, want 1",
			len(itemCandidates),
		)
	}
	if len(pageCandidates) != 1 {
		return nil, 0, 0, fmt.Errorf(
			"found %d distinct pagination states, want 1",
			len(pageCandidates),
		)
	}
	return itemCandidates[0], pageCandidates[0][0], pageCandidates[0][1], nil
}

func ofBusinessDatasetMetadata(
	infos map[string]ofBusinessRecordInfo,
	itemIDs []string,
) (int, int, error) {
	var candidates [][2]int
	matched := 0
	for datasetID, info := range infos {
		if info.ItemIDs == nil || !equalOfBusinessIDs(*info.ItemIDs, itemIDs) {
			continue
		}
		matched++
		if info.DatasetSize == nil ||
			info.DatasetSize.Total == nil ||
			info.DatasetSize.Loaded == nil {
			return 0, 0, fmt.Errorf(
				"Jobs dataset %q omitted total or loaded metadata",
				datasetID,
			)
		}
		pair := [2]int{*info.DatasetSize.Total, *info.DatasetSize.Loaded}
		if !containsOfBusinessPagePair(candidates, pair) {
			candidates = append(candidates, pair)
		}
	}
	if matched == 0 {
		return 0, 0, fmt.Errorf("no dataset metadata matches the displayed Jobs items")
	}
	if len(candidates) != 1 {
		return 0, 0, fmt.Errorf(
			"displayed Jobs items have %d conflicting dataset sizes",
			len(candidates),
		)
	}
	total, loaded := candidates[0][0], candidates[0][1]
	if total < 0 || loaded < 0 || loaded != len(itemIDs) || loaded > total {
		return 0, 0, fmt.Errorf(
			"Jobs dataset reported total=%d loaded=%d for %d displayed items",
			total, loaded, len(itemIDs),
		)
	}
	return total, loaded, nil
}

func containsOfBusinessIDs(candidates [][]string, ids []string) bool {
	for _, candidate := range candidates {
		if equalOfBusinessIDs(candidate, ids) {
			return true
		}
	}
	return false
}

func equalOfBusinessIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsOfBusinessPagePair(candidates [][2]int, pair [2]int) bool {
	for _, candidate := range candidates {
		if candidate == pair {
			return true
		}
	}
	return false
}

func ofBusinessPageLink(document, relation string) (string, bool, error) {
	var href string
	for _, match := range htmlOpenTagRe.FindAllStringSubmatch(document, -1) {
		if !strings.EqualFold(match[1], "link") {
			continue
		}
		attrs := parseHTMLAttrs(match[2])
		hasRelation := false
		for _, value := range strings.Fields(attrs["rel"]) {
			if strings.EqualFold(value, relation) {
				hasRelation = true
				break
			}
		}
		if !hasRelation {
			continue
		}
		if strings.TrimSpace(attrs["href"]) == "" {
			return "", false, fmt.Errorf("%s link omitted href", relation)
		}
		if href != "" {
			return "", false, fmt.Errorf("document contains duplicate %s links", relation)
		}
		href = attrs["href"]
	}
	return href, href != "", nil
}

func ofBusinessCategoryURL(base string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/") + "/categories")
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("expected an absolute HTTP(S) origin without credentials")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	return parsed, nil
}

func validateOfBusinessPageURL(
	categoryURL *url.URL,
	raw, queryKey string,
	page int,
) (*url.URL, error) {
	reference, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	resolved := categoryURL.ResolveReference(reference)
	if resolved.User != nil || resolved.Fragment != "" ||
		!strings.EqualFold(resolved.Scheme, categoryURL.Scheme) ||
		!strings.EqualFold(resolved.Host, categoryURL.Host) ||
		resolved.EscapedPath() != categoryURL.EscapedPath() {
		return nil, fmt.Errorf("URL %q does not stay on the careers categories page", raw)
	}
	if page == 1 {
		if resolved.RawQuery != "" || resolved.ForceQuery {
			return nil, fmt.Errorf("first-page URL %q unexpectedly has a query", raw)
		}
		return resolved, nil
	}
	query := resolved.Query()
	if resolved.ForceQuery || len(query) != 1 {
		return nil, fmt.Errorf("page URL %q must have exactly one query parameter", raw)
	}
	var actualKey string
	for key := range query {
		actualKey = key
	}
	if queryKey == "" {
		if !strings.HasSuffix(actualKey, "_page") || len(actualKey) <= len("_page") {
			return nil, fmt.Errorf("page URL %q has unexpected pagination key %q", raw, actualKey)
		}
	} else if actualKey != queryKey {
		return nil, fmt.Errorf(
			"page URL %q changed pagination key from %q to %q",
			raw, queryKey, actualKey,
		)
	}
	values := query[actualKey]
	if len(values) != 1 || values[0] != strconv.Itoa(page) {
		return nil, fmt.Errorf(
			"page URL %q identifies page %q, want %d",
			raw, strings.Join(values, ","), page,
		)
	}
	return resolved, nil
}

func resolveOfBusinessDetailURL(base, raw string) (string, error) {
	if raw != strings.TrimSpace(raw) || !strings.HasPrefix(raw, "/jobs/") {
		return "", fmt.Errorf("invalid detail path %q", raw)
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid detail path %q: %w", raw, err)
	}
	if reference.IsAbs() || reference.Host != "" || reference.User != nil ||
		reference.RawQuery != "" || reference.ForceQuery || reference.Fragment != "" {
		return "", fmt.Errorf("invalid detail path %q", raw)
	}
	siteURL, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return "", fmt.Errorf("invalid site base: %w", err)
	}
	resolved := siteURL.ResolveReference(reference)
	if !strings.EqualFold(resolved.Scheme, siteURL.Scheme) ||
		!strings.EqualFold(resolved.Host, siteURL.Host) ||
		resolved.User != nil ||
		!strings.HasPrefix(resolved.EscapedPath(), "/jobs/") {
		return "", fmt.Errorf("detail path %q leaves the careers site", raw)
	}
	return resolved.String(), nil
}

func (s *ofBusiness) fetchHTML(
	ctx context.Context,
	endpoint string,
	categoryURL *url.URL,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := clientWithoutRedirects(client).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		return nil, fmt.Errorf(
			"GET %s: %s: %s",
			endpoint, response.Status, bytes.TrimSpace(snippet),
		)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return nil, fmt.Errorf(
			"GET %s: unexpected Content-Type %q",
			endpoint, response.Header.Get("Content-Type"),
		)
	}
	if response.Request == nil || response.Request.URL == nil ||
		response.Request.URL.User != nil ||
		!strings.EqualFold(response.Request.URL.Scheme, categoryURL.Scheme) ||
		!strings.EqualFold(response.Request.URL.Host, categoryURL.Host) ||
		response.Request.URL.EscapedPath() != categoryURL.EscapedPath() {
		return nil, fmt.Errorf("GET %s: response redirected away from the careers categories page", endpoint)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ofBusinessBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", endpoint, err)
	}
	if len(body) > ofBusinessBodyLimit {
		return nil, fmt.Errorf(
			"GET %s: HTML response exceeds %d bytes",
			endpoint, ofBusinessBodyLimit,
		)
	}
	return body, nil
}
