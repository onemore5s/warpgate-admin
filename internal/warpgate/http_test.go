package warpgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPWireFormatAndErrorRedaction(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusBadRequest} {
		client, err := NewClient(Config{Host: "https://warpgate.example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var request struct {
				Options HTTPOptions `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Options.Kind != "Http" || request.Options.URL != "https://upstream.example.invalid" || request.Options.Headers["Authorization"] != "secret-placeholder" {
				t.Fatal("Incorrect HTTP API request format")
			}
			if status == http.StatusCreated {
				return jsonResponse(t, status, Target{ID: "web-id"}), nil
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"secret-placeholder"}`))}, nil
		})
		_, err = client.CreateTarget(context.Background(), TargetRequest{Name: "web", Options: HTTPOptions{Kind: HTTPKind, URL: "https://upstream.example.invalid", Headers: map[string]string{"Authorization": "secret-placeholder"}}})
		if status == http.StatusCreated && err != nil {
			t.Fatal(err)
		}
		if status != http.StatusCreated && (err == nil || strings.Contains(err.Error(), "secret-placeholder")) {
			t.Fatalf("Error response exposed credentials: %v", err)
		}
	}
}
