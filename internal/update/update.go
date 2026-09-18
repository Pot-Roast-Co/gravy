// Package update reports whether a newer released gravy exists.
//
// It answers one question — "is there something newer than what is running" — and never acts on
// the answer. Gravy does not replace its own binary: the daemon is long-lived, the TUI attaches
// to it, and a tool that swapped the executable underneath a running fleet would be choosing a
// moment its owner did not.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is GitHub's latest-release API for this project.
const DefaultEndpoint = "https://api.github.com/repos/pot-roast-co/gravy/releases/latest"

// Status is what a check found. The zero value means "nothing known", which is what every
// caller sees until the first check completes and after every failed one.
type Status struct {
	// Latest is the newest released version, e.g. "v0.1.4". Empty when unknown.
	Latest string
	// Available is true only when Latest is genuinely newer than the running build.
	Available bool
	// CheckedAt is when the answer was obtained; zero when no check has succeeded.
	CheckedAt time.Time
}

// Checker asks the release endpoint what the newest version is.
type Checker struct {
	// Endpoint is the release API to read. Empty means DefaultEndpoint.
	Endpoint string
	// Client is the HTTP client. Empty means a client with a short timeout, because this is
	// never work anyone is waiting for.
	Client *http.Client
}

// Check returns the status of current against the newest release.
//
// A failure is not an error worth surfacing: a laptop on a train cannot reach GitHub, and
// telling someone their version check failed is noise about a question they did not ask. The
// error is returned for logging and callers are expected to ignore it.
func (c Checker) Check(ctx context.Context, current string) (Status, error) {
	if !IsRelease(current) {
		// A build from source is ahead of the newest release by definition, and telling its
		// owner to "update" to the version they have already moved past is worse than silence.
		return Status{}, nil
	}

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("update: %s returned %s", endpoint, resp.Status)
	}

	var body struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Status{}, fmt.Errorf("update: decode: %w", err)
	}
	if body.Draft || body.Prerelease {
		return Status{}, nil
	}

	latest := strings.TrimSpace(body.TagName)
	if latest == "" {
		return Status{}, nil
	}
	return Status{
		Latest:    latest,
		Available: Newer(latest, current),
		CheckedAt: time.Now(),
	}, nil
}

// IsRelease reports whether v is a plain released version rather than a build from source.
//
// `git describe` stamps anything past a tag as v0.1.3-11-gd1fde28, and the Makefile's fallback
// is "dev". Both are ahead of the newest release, not behind it.
func IsRelease(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return false
	}
	nums, ok := parse(v)
	return ok && len(nums) > 0
}

// Newer reports whether latest is a later version than current.
//
// Comparison is numeric per segment, so v0.1.10 is correctly newer than v0.1.9 — a string
// compare gets that backwards, and gets it backwards precisely once a project has shipped
// enough to have users.
func Newer(latest, current string) bool {
	l, lok := parse(latest)
	c, cok := parse(current)
	if !lok || !cok {
		return false
	}
	for i := 0; i < len(l) || i < len(c); i++ {
		var a, b int
		if i < len(l) {
			a = l[i]
		}
		if i < len(c) {
			b = c[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

// parse splits a vX.Y.Z into its numbers. It refuses anything carrying a suffix, which is how
// a build from source and a pre-release are kept out of the comparison entirely.
func parse(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" || strings.ContainsAny(v, "-+ ") {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
