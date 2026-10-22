package openai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type testHTTPTransport func(*http.Request) (*http.Response, error)

func (f testHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPTransportOptionalKeyAndTypedRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, key := range []string{"", "test-key"} {
			client := New(key, WithHTTPTransport(testHTTPTransport(func(request *http.Request) (*http.Response, error) {
				_, present := request.Header["Authorization"]
				if present != (key != "") {
					t.Errorf("optional authorization: present=%v", present)
				}
				if key != "" && request.Header.Get("Authorization") != "Bearer "+key {
					t.Error("key changed")
				}
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"3"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
			})))
			var err error
			if stream {
				_, err = client.Stream(t.Context(), contract.ChatRequest{Model: "m"})
			} else {
				_, err = client.Chat(t.Context(), contract.ChatRequest{Model: "m"})
			}
			var rejected *HTTPError
			if !errors.As(err, &rejected) || rejected.StatusCode != 429 || rejected.RetryAfter != "3" || rejected.Body != "busy" {
				t.Fatalf("typed rejection: %v", err)
			}
		}
	}
}
