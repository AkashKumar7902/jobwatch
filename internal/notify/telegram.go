package notify

// The "telegram" notifier sends each batch through a Telegram bot. Create
// a bot with @BotFather, start a chat with it (or add it to a group), and
// find the chat id via https://api.telegram.org/bot<token>/getUpdates.
//
// Config:
//
//	- name: telegram
//	  params:
//	    token_env: JOBWATCH_TELEGRAM_TOKEN  # bot token, or token: 123:abc
//	    chat_id: "123456789"

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
	Register("telegram", func(p params.Map) (Notifier, error) {
		token := p.Get("token")
		if envName := p.Get("token_env"); envName != "" {
			token = os.Getenv(envName)
			if token == "" {
				return nil, fmt.Errorf("token_env %s is set in config but the environment variable is empty", envName)
			}
		}
		if token == "" {
			return nil, fmt.Errorf(`telegram needs "token" or "token_env"`)
		}
		chatID, err := p.Require("chat_id")
		if err != nil {
			return nil, err
		}
		return &telegram{
			// api_base is overridable for tests.
			base:   p.GetDefault("api_base", "https://api.telegram.org") + "/bot" + token,
			chatID: chatID,
			client: &http.Client{Timeout: 30 * time.Second},
		}, nil
	})
}

type telegram struct {
	base   string // https://api.telegram.org/bot<token>
	chatID string
	client *http.Client
}

func (t *telegram) Name() string { return "telegram" }

// Telegram caps messages at 4096 characters; batches are split on job
// boundaries into chunks below that.
const telegramChunkLimit = 3900

func (t *telegram) Notify(ctx context.Context, matches []Match) error {
	chunk := Headline(matches) + "\n"
	for i, m := range matches {
		block := "\n" + Block(i+1, m)
		if len(chunk)+len(block) > telegramChunkLimit && chunk != "" {
			if err := t.send(ctx, chunk); err != nil {
				return err
			}
			chunk = ""
		}
		chunk += block
	}
	if strings.TrimSpace(chunk) != "" {
		return t.send(ctx, chunk)
	}
	return nil
}

func (t *telegram) send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  t.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var reply struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err := json.Unmarshal(raw, &reply); err != nil || !reply.OK {
		return fmt.Errorf("telegram sendMessage failed (%s): %s", resp.Status, firstNonEmpty(reply.Description, string(bytes.TrimSpace(raw))))
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
