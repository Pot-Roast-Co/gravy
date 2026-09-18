package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.1.4", "v0.1.3", true},
		{"v0.1.3", "v0.1.3", false},
		{"v0.1.3", "v0.1.4", false},
		{"v0.2.0", "v0.1.9", true},
		{"v1.0.0", "v0.9.9", true},
		// The one a string compare gets backwards, and gets backwards exactly when a project
		// has shipped enough releases to have users.
		{"v0.1.10", "v0.1.9", true},
		{"v0.1.9", "v0.1.10", false},
		// Shorter is not smaller: v0.2 and v0.2.0 are the same version.
		{"v0.2", "v0.2.0", false},
		{"v0.2.1", "v0.2", true},
		// A build from source never counts as behind.
		{"v0.1.3", "v0.1.3-11-gd1fde28", false},
		{"v0.1.4", "dev", false},
		{"", "v0.1.3", false},
	} {
		if got := Newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

// A build from source is ahead of the newest release, not behind it, and must never be nagged.
func TestIsRelease(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"v0.1.3", true},
		{"0.1.3", true},
		{"v0.1.3-11-gd1fde28", false},
		{"v0.1.4-snapshot-abc1234", false},
		{"dev", false},
		{"", false},
	} {
		if got := IsRelease(tc.in); got != tc.want {
			t.Errorf("IsRelease(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func serving(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCheckFindsANewerRelease(t *testing.T) {
	s := serving(t, `{"tag_name":"v0.1.4","draft":false,"prerelease":false}`)

	got, err := Checker{Endpoint: s.URL}.Check(context.Background(), "v0.1.3")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Latest != "v0.1.4" {
		t.Fatalf("status = %+v, want v0.1.4 available", got)
	}
	if got.CheckedAt.IsZero() {
		t.Error("a successful check recorded no time")
	}
}

func TestCheckOnTheCurrentReleaseOffersNothing(t *testing.T) {
	s := serving(t, `{"tag_name":"v0.1.3"}`)

	got, err := Checker{Endpoint: s.URL}.Check(context.Background(), "v0.1.3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Fatalf("offered an update to the version already running: %+v", got)
	}
	if got.Latest != "v0.1.3" {
		t.Errorf("latest = %q; the answer is still worth reporting", got.Latest)
	}
}

// A build from source does not call out at all: there is nothing to tell its owner, so there is
// no reason to make the request.
func TestCheckFromSourceMakesNoRequest(t *testing.T) {
	called := false
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer s.Close()

	got, err := Checker{Endpoint: s.URL}.Check(context.Background(), "v0.1.3-11-gd1fde28")
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a source build asked GitHub about releases")
	}
	if got.Available || got.Latest != "" {
		t.Errorf("status = %+v, want nothing known", got)
	}
}

// Pre-releases must not become the version everyone is told to move to, matching the cask's
// skip_upload: auto.
func TestCheckIgnoresPrereleases(t *testing.T) {
	s := serving(t, `{"tag_name":"v0.2.0","prerelease":true}`)

	got, err := Checker{Endpoint: s.URL}.Check(context.Background(), "v0.1.3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Fatalf("offered a pre-release: %+v", got)
	}
}

// An unreachable endpoint is silence, not a visible failure: it is an answer to a question
// nobody asked, and a laptop on a train cannot reach GitHub.
func TestCheckFailureIsQuiet(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s.Close()

	got, err := Checker{Endpoint: s.URL}.Check(context.Background(), "v0.1.3")
	if err == nil {
		t.Error("want an error for the log")
	}
	if got.Available || got.Latest != "" {
		t.Errorf("a failed check invented a status: %+v", got)
	}
}
