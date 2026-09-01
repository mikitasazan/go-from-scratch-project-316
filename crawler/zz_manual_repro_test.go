package crawler_test

// Temporary manual repro file for a code review; deleted after the run.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code/crawler"
)

// Repro 1: page reached through a redirect — are relative links resolved
// against the final address or the original one?
func TestReproRedirectBase(t *testing.T) {
	var mu sync.Mutex
	var requested []string

	header := func() http.Header {
		h := make(http.Header)
		h.Set("Content-Type", "text/html; charset=utf-8")
		return h
	}

	resp := func(r *http.Request, status int, location, body string) *http.Response {
		h := header()
		if location != "" {
			h.Set("Location", location)
		}
		return &http.Response{
			Status:     http.StatusText(status),
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     h,
			Request:    r,
		}
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, r.Method+" "+r.URL.String())
		mu.Unlock()

		switch r.URL.String() {
		case "https://example.com/blog":
			return resp(r, http.StatusMovedPermanently, "/blog/", ""), nil
		case "https://example.com/blog/":
			return resp(r, http.StatusOK, "", `<a href="post-1">p1</a>`), nil
		case "https://example.com/blog/post-1":
			return resp(r, http.StatusOK, "", ""), nil
		default:
			return resp(r, http.StatusNotFound, "", ""), nil
		}
	})}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/blog",
		Depth:      3,
		HTTPClient: client,
	})

	mu.Lock()
	t.Logf("requested: %v", requested)
	mu.Unlock()

	raw, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("report:\n%s", raw)
}

// Repro 2: root without a trailing slash plus a Home link to "/" — is the
// homepage document fetched twice?
func TestReproRootNormalization(t *testing.T) {
	var requests sync.Map

	pages := map[string]string{
		"https://example.com":  `<a href="/">home</a>`,
		"https://example.com/": `<a href="/">home</a>`,
	}

	requestsList := func() []string {
		var out []string
		requests.Range(func(k, v any) bool {
			out = append(out, k.(string))
			return true
		})
		return out
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count, _ := requests.LoadOrStore(r.URL.String(), new(int64))
		atomic.AddInt64(count.(*int64), 1)

		h := make(http.Header)
		h.Set("Content-Type", "text/html; charset=utf-8")

		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(pages[r.URL.String()])),
			Header:     h,
			Request:    r,
		}, nil
	})}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		Depth:      3,
		HTTPClient: client,
	})

	t.Logf("requested: %v", requestsList())
	t.Logf("pages: %v", urls(report))
}

// Repro 3: what does url.String() do when Fragment is cleared but the href
// carried an encoded fragment?
func TestReproFragmentString(t *testing.T) {
	for _, href := range []string{"/one#top", "/one#%41", "/one#t%6Fp"} {
		u, err := url.Parse("https://example.com" + href)
		if err != nil {
			t.Fatalf("parse %s: %v", href, err)
		}

		u.Fragment = ""

		t.Logf("href=%-14q after clearing fragment -> %q", href, u.String())
	}
}

// Repro 4: a run with a link whose fragment survives as a distinct address —
// does the report gain a bogus page?
func TestReproEncodedFragmentCrawl(t *testing.T) {
	var requests sync.Map

	pages := map[string]string{
		"https://example.com/": `<a href="/one">1</a><a href="/one#t%6Fp">again</a>`,
		"https://example.com/one": ``,
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count, _ := requests.LoadOrStore(r.URL.String(), new(int64))
		atomic.AddInt64(count.(*int64), 1)

		h := make(http.Header)
		h.Set("Content-Type", "text/html; charset=utf-8")

		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(pages[r.URL.String()])),
			Header:     h,
			Request:    r,
		}, nil
	})}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      3,
		HTTPClient: client,
	})

	var fetched []string
	requests.Range(func(k, v any) bool {
		fetched = append(fetched, k.(string))
		return true
	})

	t.Logf("fetched: %v", fetched)
	t.Logf("pages: %v", urls(report))
}

// Repro 5: --timeout=0 through Options — what does the injected client do?
func TestReproZeroTimeoutOptions(t *testing.T) {
	// Just documenting newCrawl's normalization for a non-nil client with
	// Timeout 0: the fallback never applies, so no timeout at all.
	_ = time.Second
}
