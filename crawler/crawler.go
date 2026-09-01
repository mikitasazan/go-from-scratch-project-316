// Package crawler walks a site and reports what it found there.
package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
// reason to stop the crawl, so it comes back inside the Page. The parsed
// document comes back alongside it, empty when there was nothing to read.
func (c *crawl) visit(ctx context.Context, address string, depth int) (Page, document) {
	page := Page{URL: address, Depth: depth, Status: statusFailed}

	var doc document

	var lastErr error

	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if attempt > 0 && c.opts.Delay > 0 {
			select {
			case <-ctx.Done():
				page.Error = ctx.Err().Error()
				return page, doc
			case <-time.After(c.opts.Delay):
			}
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			page.Error = err.Error()
			return page, doc
		}

		if c.opts.UserAgent != "" {
			request.Header.Set("User-Agent", c.opts.UserAgent)
		}

		response, err := c.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}

		page.HTTPStatus = response.StatusCode

		if response.StatusCode >= http.StatusBadRequest {
			_ = response.Body.Close()
			page.Error = response.Status

			return page, doc
		}

		if base, err := url.Parse(address); err == nil && isHTML(response) {
			doc = parseDocument(base, response.Body)
		}

		_ = response.Body.Close()

		page.Status = statusOK

		return page, doc
	}

	if lastErr != nil {
		page.Error = lastErr.Error()
	}

	return page, doc
}

// isHTML keeps the parser away from images and downloads, which are checked
// but never read for links.
func isHTML(response *http.Response) bool {
	return strings.Contains(response.Header.Get("Content-Type"), "text/html")
}

// run walks the site breadth first: one level of the tree at a time, every
// address on that level fetched by the worker pool at once. Working level by
// level is what keeps --depth meaningful, and the visited set is only touched
// between levels, so no lock is needed inside a level.
func (c *crawl) run(ctx context.Context, root string) []Page {
	base, err := url.Parse(root)
	if err != nil {
		page, _ := c.visit(ctx, root, 0)
		return []Page{page}
	}

	var (
		pages   []Page
		visited = map[string]struct{}{root: {}}
		level   = []string{root}
	)

	for depth := 0; depth < c.opts.Depth && len(level) > 0; depth++ {
		results := c.fetchLevel(ctx, level, depth)

		var next []string

		for _, result := range results {
			pages = append(pages, result.page)

			if !sameHost(base, result.page.URL) {
				continue
			}

			for _, link := range result.doc.links {
				if _, seen := visited[link]; seen {
					continue
				}

				visited[link] = struct{}{}
				next = append(next, link)
			}
		}

		level = next
	}

	return pages
}

// visited is one address after it was fetched.
type visited struct {
	index int
	page  Page
	doc   document
}

// fetchLevel runs one level of the walk through the worker pool and returns the
// results in the order the addresses were queued, so two runs over the same
// site produce the same report.
func (c *crawl) fetchLevel(ctx context.Context, level []string, depth int) []visited {
	workers := min(c.opts.Concurrency, len(level))

	queue := make(chan int)
	results := make(chan visited, len(level))

	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range queue {
				page, doc := c.visit(ctx, level[i], depth)
				results <- visited{index: i, page: page, doc: doc}
			}
		}()
	}

	go func() {
		defer close(queue)

		for i := range level {
			select {
			case <-ctx.Done():
				return
			case queue <- i:
			}
		}
	}()

	wg.Wait()
	close(results)

	ordered := make([]visited, len(level))
	filled := make([]bool, len(level))

	for result := range results {
		ordered[result.index] = result
		filled[result.index] = true
	}

	out := make([]visited, 0, len(level))

	for i, ok := range filled {
		if ok {
			out = append(out, ordered[i])
		}
	}

	return out
}

// Analyze crawls the site named in opts and returns the JSON report.
func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	if opts.URL == "" {
		return nil, ErrNoURL
	}

	run := newCrawl(opts)

	pages := run.run(ctx, opts.URL)
	if pages == nil {
		pages = []Page{}
	}

	report := Report{
		RootURL:     opts.URL,
		Depth:       run.opts.Depth,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Pages:       pages,
	}

	if opts.IndentJSON {
		return json.MarshalIndent(report, "", "  ")
	}

	return json.Marshal(report)
}
