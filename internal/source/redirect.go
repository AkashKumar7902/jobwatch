package source

import "net/http"

// clientWithoutRedirects preserves the caller's transport, cookie jar, and
// timeout while stopping redirects before a second outbound request is sent.
// Sources can then validate the 3xx response as an ordinary protocol error.
func clientWithoutRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}
