package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
)

// Client holds the context for requests to the Twitch API.
type Client struct {
	HTTP  *http.Client
	Token *oauth2.Token
}

// reqjson performs an HTTP request and decodes the response as JSON.
// The response body is truncated to 2 MB.
func reqjson[Resp any](ctx context.Context, client Client, method, url string, body io.Reader, u *Resp) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("couldn't make request: %w", err)
	}
	client.Token.SetAuthHeader(req)
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't %s: %w", method, err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("couldn't read response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request failed: %s (%s)", b, resp.Status)
	}
	r := struct {
		Data *Resp `json:"data"`
	}{u}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("couldn't decode JSON response: %w", err)
	}
	return nil
}

// apiurl creates an api.twitch.tv URL for the given endpoint and with the
// given URL parameters.
func apiurl(ep string, values url.Values) string {
	u, err := url.JoinPath("https://api.twitch.tv/", ep)
	if err != nil {
		panic("twitch: bad url join with " + ep)
	}
	if len(values) == 0 {
		return u
	}
	return u + "?" + values.Encode()
}
