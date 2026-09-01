// Package crawler walks a site and reports what it found there.
package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const (
	// DefaultDepth and friends mirror the CLI defaults, so calling Analyze
	// directly behaves the way the command does.
	DefaultDepth       = 10
	DefaultRetries     = 1
	DefaultTimeout     = 15 * time.Second
	DefaultConcurrency = 4

	statusOK     = "ok"
	statusFailed = "failed"
)

// ErrNoURL is returned when Options carries no address to start from.
var ErrNoURL = errors.New("url is required")

// Options carries everything one crawl run needs. HTTPClient is part of it on
// purpose: the network is passed in, never reached for, so a test can hand the
// crawler a client of its own.
type Options struct {
	URL         string
	Depth       int
	Retries     int
	Delay       time.Duration
	Timeout     time.Duration
	UserAgent   string
	Concurrency int
	IndentJSON  bool
	HTTPClient  *http.Client
}

// Page is one address the crawl visited.
type Page struct {
	URL        string `json:"url"`
	Depth      int    `json:"depth"`
	HTTPStatus int    `json:"http_status"`
	Status     string `json:"status"`
	Error      string `json:"error"`
}

// Report is the JSON document a crawl produces.
type Report struct {
	RootURL     string `json:"root_url"`
	Depth       int    `json:"depth"`
	GeneratedAt string `json:"generated_at"`
	Pages       []Page `json:"pages"`
}

// crawl holds the state of a single run: the client to use, the options it was
// given, and what it has seen so far.
type crawl struct {
	opts   Options
	client *http.Client
}

func newCrawl(opts Options) *crawl {
	if opts.Depth <= 0 {
		opts.Depth = DefaultDepth
	}

	if opts.Retries < 0 {
		opts.Retries = DefaultRetries
	}

	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}

	return &crawl{opts: opts, client: client}
}

// visit fetches one address. A network failure is a fact about the page, not a
// reason to stop the crawl, so it comes back inside the Page.
func (c *crawl) visit(ctx context.Context, url string, depth int) Page {
	page := Page{URL: url, Depth: depth, Status: statusFailed}

	var lastErr error

	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if attempt > 0 && c.opts.Delay > 0 {
			select {
			case <-ctx.Done():
				page.Error = ctx.Err().Error()
				return page
			case <-time.After(c.opts.Delay):
			}
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			page.Error = err.Error()
			return page
		}

		if c.opts.UserAgent != "" {
			request.Header.Set("User-Agent", c.opts.UserAgent)
		}

		response, err := c.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}

		_ = response.Body.Close()

		page.HTTPStatus = response.StatusCode
		if response.StatusCode >= http.StatusBadRequest {
			page.Error = response.Status
			return page
		}

		page.Status = statusOK

		return page
	}

	if lastErr != nil {
		page.Error = lastErr.Error()
	}

	return page
}

// Analyze crawls the site named in opts and returns the JSON report.
func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	if opts.URL == "" {
		return nil, ErrNoURL
	}

	run := newCrawl(opts)

	report := Report{
		RootURL:     opts.URL,
		Depth:       run.opts.Depth,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Pages:       []Page{run.visit(ctx, opts.URL, 0)},
	}

	if opts.IndentJSON {
		return json.MarshalIndent(report, "", "  ")
	}

	return json.Marshal(report)
}
