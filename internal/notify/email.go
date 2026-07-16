package notify

// The "email" notifier sends one plain-text email per run summarizing all
// new matches. It speaks SMTP with STARTTLS (port 587: Gmail app
// passwords, SES, Mailgun, ...) or implicit TLS (port 465), with real
// connection deadlines so a stalled server can't hang the watcher.
//
// Config:
//
//	- name: email
//	  params:
//	    smtp_host: smtp.gmail.com
//	    smtp_port: 587                         # 465 switches to implicit TLS
//	    username: you@gmail.com
//	    password_env: JOBWATCH_SMTP_PASSWORD   # read from env, keeps secrets out of the file
//	    from: you@gmail.com
//	    to: you@example.com                    # comma-separated for several recipients

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"jobwatch/internal/params"
)

func init() {
	Register("email", func(p params.Map) (Notifier, error) {
		host, err := p.Require("smtp_host")
		if err != nil {
			return nil, err
		}
		from, err := requireAddress(p, "from")
		if err != nil {
			return nil, err
		}
		toRaw, err := p.Require("to")
		if err != nil {
			return nil, err
		}
		var to []string
		for _, raw := range strings.Split(toRaw, ",") {
			if raw = strings.TrimSpace(raw); raw == "" {
				continue
			}
			addr, err := mail.ParseAddress(raw)
			if err != nil {
				return nil, fmt.Errorf("param \"to\": invalid address %q: %w", raw, err)
			}
			to = append(to, addr.Address)
		}
		if len(to) == 0 {
			return nil, fmt.Errorf("param \"to\": no recipient addresses")
		}

		password := p.Get("password")
		if envName := p.Get("password_env"); envName != "" {
			password = os.Getenv(envName)
			if password == "" {
				return nil, fmt.Errorf("password_env %s is set in config but the environment variable is empty", envName)
			}
		}

		port := p.GetDefault("smtp_port", "587")
		return &email{
			addr:        host + ":" + port,
			host:        host,
			implicitTLS: port == "465",
			username:    p.Get("username"),
			password:    password,
			from:        from,
			to:          to,
			prefix:      p.GetDefault("subject_prefix", "[jobwatch] "),
		}, nil
	})
}

func requireAddress(p params.Map, key string) (string, error) {
	raw, err := p.Require(key)
	if err != nil {
		return "", err
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", fmt.Errorf("param %q: invalid address %q: %w", key, raw, err)
	}
	return addr.Address, nil
}

type email struct {
	addr        string // host:port
	host        string
	implicitTLS bool
	username    string
	password    string
	from        string
	to          []string
	prefix      string
}

func (e *email) Name() string { return "email" }

// sendTimeout bounds the whole SMTP conversation so a silent or blackholed
// server can't stall the watcher forever.
const sendTimeout = 2 * time.Minute

func (e *email) Notify(ctx context.Context, matches []Match) error {
	msg := e.compose(matches)
	if err := e.send(ctx, msg); err != nil {
		return fmt.Errorf("sending mail via %s: %w", e.addr, err)
	}
	return nil
}

func (e *email) compose(matches []Match) []byte {
	subject := OneLine(e.prefix) + Headline(matches)
	body := Headline(matches) + " for your experience criteria.\n\n" + Text(matches) + "\n- jobwatch\n"

	msg := strings.Join([]string{
		"From: " + e.from,
		"To: " + strings.Join(e.to, ", "),
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		strings.ReplaceAll(body, "\n", "\r\n"),
	}, "\r\n")
	return []byte(msg)
}

// send performs the SMTP conversation with an overall deadline, honoring
// ctx cancellation (Ctrl-C aborts an in-flight send).
func (e *email) send(ctx context.Context, msg []byte) error {
	dialer := net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", e.addr)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		conn.Close()
		return err
	}
	// Kill the connection if ctx is cancelled mid-conversation; the
	// blocked read/write then returns immediately.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if e.implicitTLS {
		conn = tls.Client(conn, &tls.Config{ServerName: e.host})
	}
	c, err := smtp.NewClient(conn, e.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if !e.implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: e.host}); err != nil {
				return err
			}
		} else if e.password != "" {
			return fmt.Errorf("server does not offer STARTTLS; refusing to send credentials over plaintext")
		}
	}
	if e.username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.username, e.password, e.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(e.from); err != nil {
		return err
	}
	for _, rcpt := range e.to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
