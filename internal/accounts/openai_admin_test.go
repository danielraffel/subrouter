package accounts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFlexibleFloat64AcceptsStringAndNumber(t *testing.T) {
	var got struct {
		Number flexibleFloat64 `json:"number"`
		String flexibleFloat64 `json:"string"`
	}
	if err := json.Unmarshal([]byte(`{"number":12.5,"string":"3.75"}`), &got); err != nil {
		t.Fatal(err)
	}
	if float64(got.Number) != 12.5 {
		t.Fatalf("number = %v, want 12.5", got.Number)
	}
	if float64(got.String) != 3.75 {
		t.Fatalf("string = %v, want 3.75", got.String)
	}
}

func TestFetchOpenAIUsageRollupStartsRequestsConcurrently(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	released := false
	releaseRequests := func() {
		if !released {
			close(release)
			released = true
		}
	}
	defer releaseRequests()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		started <- req.URL.Path + ":" + req.URL.Query().Get("bucket_width")
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Request:    req,
		}, nil
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := FetchOpenAIUsageRollup(ctx, client, "sk-admin-test-placeholder", 7, "")
		done <- err
	}()

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case key := <-started:
			seen[key] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("requests did not start concurrently; saw %v", seen)
		}
	}

	for _, key := range []string{
		"/v1/organization/costs:1d",
		"/v1/organization/usage/completions:1d",
		"/v1/organization/usage/completions:1h",
	} {
		if !seen[key] {
			t.Fatalf("missing request %s; saw %v", key, seen)
		}
	}

	releaseRequests()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollup did not finish after releasing requests")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
