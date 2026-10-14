package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbe is the three answers the probe has to tell apart: the function is
// there, the function is not enabled, and the server did not answer.
func TestProbe(t *testing.T) {
	t.Run("the function is there", func(t *testing.T) {
		client, rec := serve(t, http.StatusOK, "vector_empty.json")

		got, err := client.Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if !got.ExperimentalFunctions {
			t.Error("ExperimentalFunctions = false, want true: the server answered the query")
		}
		if len(rec.requests) != 1 {
			t.Fatalf("got %d requests, want 1: the probe asks once", len(rec.requests))
		}
		if query := rec.requests[0].Get("query"); !strings.Contains(query, "mad_over_time") {
			t.Errorf("query = %q, want it to use mad_over_time", query)
		}
	})

	// Two rejections mean the same thing and they are not the same sentence.
	// A Prometheus that has the function behind the flag says it is not
	// enabled; a server whose parser never had it says it does not know the
	// name. Both are the capability being absent.
	for _, fixture := range []string{"error_function_not_enabled.json", "error_unknown_function.json"} {
		t.Run("the function is unavailable: "+fixture, func(t *testing.T) {
			client, _ := serve(t, http.StatusBadRequest, fixture)

			got, err := client.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: %v: an unavailable function is an answer, not a failure", err)
			}
			if got.ExperimentalFunctions {
				t.Error("ExperimentalFunctions = true, want false")
			}
		})
	}

	t.Run("a query rejected for another reason", func(t *testing.T) {
		client, _ := serve(t, http.StatusBadRequest, "error_bad_query.json")

		if _, err := client.Probe(context.Background()); err == nil {
			t.Error("Probe succeeded: a rejection that does not name the function is a failure")
		}
	})

	t.Run("a proxy in the way", func(t *testing.T) {
		client, _ := serve(t, http.StatusGatewayTimeout, "gateway_timeout.html")

		_, err := client.Probe(context.Background())
		if err == nil {
			t.Fatal("Probe succeeded against a 504")
		}
		if !strings.Contains(err.Error(), "mad_over_time") {
			t.Errorf("error %q does not say what was being probed", err)
		}
	})

	t.Run("a server that is not listening", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		client, err := New(srv.URL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		client.HTTP = srv.Client()
		srv.Close()

		if _, err := client.Probe(context.Background()); err == nil {
			t.Error("Probe succeeded against a closed server")
		}
	})
}
