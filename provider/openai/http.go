package openai

import (
	"fmt"
	"net/http"
)

// HTTPError exposes status and retry advice without requiring error-string
// parsing. It describes an HTTP rejection, not a partially consumed stream.
type HTTPError struct {
	StatusCode int
	RetryAfter string
	Body       string
}

func (e *HTTPError) Error() string           { return fmt.Sprintf("openai: HTTP %d: %s", e.StatusCode, e.Body) }
func (e *HTTPError) HTTPStatus() int         { return e.StatusCode }
func (e *HTTPError) RetryAfterValue() string { return e.RetryAfter }

func responseError(response *http.Response, body []byte) error {
	if len(body) > 4096 {
		body = body[:4096]
	}
	return &HTTPError{StatusCode: response.StatusCode, RetryAfter: response.Header.Get("Retry-After"), Body: string(body)}
}

// WithHTTPTransport lets an embedding application own connection pooling.
// The client retains its attempt timeout and request cancellation behavior.
func WithHTTPTransport(transport http.RoundTripper) Option {
	return func(c *Client) {
		if transport != nil {
			c.client.Transport = transport
		}
	}
}
