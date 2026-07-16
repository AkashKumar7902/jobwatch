package notify

// The "webhook" notifier POSTs each batch to an HTTP endpoint. One
// notifier covers many services through the format param:
//
//	format: slack    {"text": "..."}      Slack incoming webhooks (Mattermost and Rocket.Chat accept the same shape)
//	format: discord  {"content": "..."}   Discord webhooks
//	format: text     plain-text body      ntfy.sh and home-grown endpoints
//	format: json     structured payload   anything custom
//
// Config:
//
//	- name: webhook
//	  params:
//	    url_env: JOBWATCH_SLACK_WEBHOOK  # or url: https://hooks.slack.com/...
//	    format: slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"jobwatch/internal/params"
)

func init() {
	Register("webhook", func(p params.Map) (Notifier, error) {
		url := p.Get("url")
		if envName := p.Get("url_env"); envName != "" {
			url = os.Getenv(envName)
			if url == "" {
				return nil, fmt.Errorf("url_env %s is set in config but the environment variable is empty", envName)
			}
		}
		if url == "" {
			return nil, fmt.Errorf(`webhook needs "url" or "url_env"`)
		}
		format := p.GetDefault("format", "json")
		switch format {
		case "slack", "discord", "text", "json":
		default:
			return nil, fmt.Errorf("unknown webhook format %q (want slack, discord, text, or json)", format)
		}
		return &webhook{
			url:    url,
			format: format,
			client: &http.Client{Timeout: 30 * time.Second},
		}, nil
	})
}

type webhook struct {
	url    string
	format string
	client *http.Client
}

func (w *webhook) Name() string { return "webhook(" + w.format + ")" }

func (w *webhook) Notify(ctx context.Context, matches []Match) error {
	body, contentType, err := w.payload(matches)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("webhook returned %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}
	return nil
}

func (w *webhook) payload(matches []Match) (body []byte, contentType string, err error) {
	text := Headline(matches) + "\n\n" + Text(matches)

	switch w.format {
	case "text":
		return []byte(text), "text/plain; charset=utf-8", nil
	case "slack":
		body, err = json.Marshal(map[string]string{"text": text})
	case "discord":
		// Discord rejects messages over 2000 characters.
		body, err = json.Marshal(map[string]string{"content": truncate(text, 1900)})
	case "json":
		type jobPayload struct {
			Company  string `json:"company"`
			Title    string `json:"title"`
			Location string `json:"location,omitempty"`
			URL      string `json:"url"`
			Reason   string `json:"reason"`
		}
		jobs := make([]jobPayload, 0, len(matches))
		for _, m := range matches {
			jobs = append(jobs, jobPayload{
				Company:  m.Job.Company,
				Title:    m.Job.Title,
				Location: m.Job.Location,
				URL:      m.Job.URL,
				Reason:   m.Reason,
			})
		}
		body, err = json.Marshal(map[string]any{"headline": Headline(matches), "matches": jobs})
	}
	return body, "application/json", err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n…(truncated)"
}
