package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jobwatch/internal/model"
	"jobwatch/internal/params"
)

var sample = []Match{
	{Job: model.Job{Company: "Acme", Title: "Junior Dev", Location: "Remote", URL: "https://a/1"}, Reason: "asks for ~1 year"},
	{Job: model.Job{Company: "Umbrella", Title: "Support\r\nEngineer", URL: "https://u/2"}, Reason: "entry level"},
}

func TestOneLineDefusesControlChars(t *testing.T) {
	got := OneLine("a\r\nSubject: hacked\x00b")
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Errorf("control characters survived: %q", got)
	}
}

func TestTextContainsAllFields(t *testing.T) {
	text := Text(sample)
	for _, want := range []string{"Acme", "Junior Dev", "Remote", "https://a/1", "asks for ~1 year", "Umbrella"} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\r") {
		t.Error("Text() should not contain CR from job data")
	}
}

func newWebhook(t *testing.T, url, format string) Notifier {
	t.Helper()
	n, err := New("webhook", params.Map{"url": url, "format": format})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebhookFormats(t *testing.T) {
	var gotBody []byte
	var gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotType = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	t.Run("slack", func(t *testing.T) {
		if err := newWebhook(t, srv.URL, "slack").Notify(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
		var payload map[string]string
		if err := json.Unmarshal(gotBody, &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload["text"], "2 new matching job(s)") {
			t.Errorf("slack text missing headline: %q", payload["text"])
		}
	})

	t.Run("json", func(t *testing.T) {
		if err := newWebhook(t, srv.URL, "json").Notify(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Headline string `json:"headline"`
			Matches  []struct {
				Company string `json:"company"`
				URL     string `json:"url"`
			} `json:"matches"`
		}
		if err := json.Unmarshal(gotBody, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Matches) != 2 || payload.Matches[0].Company != "Acme" {
			t.Errorf("unexpected json payload: %+v", payload)
		}
	})

	t.Run("text", func(t *testing.T) {
		if err := newWebhook(t, srv.URL, "text").Notify(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(gotType, "text/plain") {
			t.Errorf("content type = %q", gotType)
		}
	})
}

func TestWebhookReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such hook", http.StatusNotFound)
	}))
	defer srv.Close()
	err := newWebhook(t, srv.URL, "slack").Notify(context.Background(), sample)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 error, got %v", err)
	}
}

func TestWebhookRejectsUnknownFormat(t *testing.T) {
	if _, err := New("webhook", params.Map{"url": "https://x", "format": "carrier-pigeon"}); err == nil {
		t.Error("unknown format should be rejected at config time")
	}
}

func TestTelegramSendsAndChunks(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/bottok123/sendMessage") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		requests = append(requests, m)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	n, err := New("telegram", params.Map{"token": "tok123", "chat_id": "42", "api_base": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0]["chat_id"] != "42" {
		t.Fatalf("unexpected requests: %+v", requests)
	}

	// A huge batch must split into several messages.
	requests = nil
	big := make([]Match, 60)
	for i := range big {
		big[i] = Match{Job: model.Job{Company: "C", Title: strings.Repeat("x", 100), URL: "https://x"}, Reason: strings.Repeat("y", 100)}
	}
	if err := n.Notify(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 {
		t.Errorf("expected chunking into multiple messages, got %d", len(requests))
	}
}

func TestTelegramSurfacesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"description":"chat not found"}`)
	}))
	defer srv.Close()
	n, err := New("telegram", params.Map{"token": "t", "chat_id": "1", "api_base": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = n.Notify(context.Background(), sample)
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("expected telegram API error, got %v", err)
	}
}
