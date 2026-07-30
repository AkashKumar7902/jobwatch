package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// postJSONValue marshals a JSON request value and decodes the JSON response.
// It keeps GraphQL and form-shaped JSON sources from hand-building payloads.
func postJSONValue(ctx context.Context, client *http.Client, endpoint string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encoding request for %s: %w", endpoint, err)
	}
	return fetchJSON(ctx, client, http.MethodPost, endpoint, body, response)
}
