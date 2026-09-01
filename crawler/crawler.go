// Package crawler walks a site and reports what it found there.
package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

	// retryPause is the wait before another attempt. It is deliberately not
	// zero: repeating instantly is another burst at a server that is already
	// struggling.
	retryPause = 200 * time.Millisecond
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
	RPS         float64
	Timeout     time.Duration
	UserAgent   string
	Concurrency int
	IndentJSON  bool
	HTTPClient  *http.Client
}

// BrokenLink is one address a page points at that does not answer: either the
// server refused it (StatusCode) or the request never got through (Error).
type BrokenLink struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
}

// Asset is one file a page pulls in, with what came back when it was asked for.
type Asset struct {
	URL        string `json:"url"`
	Type       string `json:"type"`
	StatusCode int    `json:"status_code"`
	SizeBytes  int64  `json:"size_bytes"`
	Error      string `json:"error"`
}

// Page is one address the crawl visited.
type Page struct {
	URL          string       `json:"url"`
	Depth        int          `json:"depth"`
	HTTPStatus   int          `json:"http_status"`
	Status       string       `json:"status"`
	Error        string       `json:"error"`
	SEO          SEO          `json:"seo"`
	BrokenLinks  []BrokenLink `json:"broken_links"`
	Assets       []Asset      `json:"assets"`
	DiscoveredAt string       `json:"discovered_at"`
}

// Report is the JSON document a crawl produces.
type Report struct {
	RootURL     string `json:"root_url"`
	Depth       int    `json:"depth"`
	GeneratedAt string `json:"generated_at"`
	Pages       []Page `json:"pages"`
}

// probe is the outcome of asking one address whether it is there.
type probe struct {
	statusCode int
	err        error
}

func (p probe) broken() bool {
	return p.err != nil || p.statusCode >= http.StatusBadRequest
}

// crawl holds the state of a single run: the client to use, the options it was
// given, and what it has already asked about.
type crawl struct {
	opts    Options
	client  *http.Client
	base    *url.URL
	limiter *limiter

	mu     sync.Mutex
	probes map[string]probe
	assets map[string]Asset
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

	return &crawl{
		opts:    opts,
		client:  client,
		limiter: newLimiter(interval(opts.RPS, opts.Delay)),
		probes:  map[string]probe{},
		assets:  map[string]Asset{},
	}
}

// temporary says whether a status code is worth asking about again. A 404 will
// still be a 404 on the second try; an overloaded server may not be.
func temporary(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// request performs one HTTP call, repeating it up to Retries more times while
// the problem looks temporary — a request that never reached the server, or a
// server that said it is overloaded. Any other answer is returned as it is.
func (c *crawl) request(ctx context.Context, method, address string) (*http.Response, error) {
	var (
		lastResponse *http.Response
		lastErr      error
	)

	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if attempt > 0 {
			if err := pause(ctx, retryPause); err != nil {
				break
			}
		}

		if err := c.limiter.wait(ctx); err != nil {
			break
		}

		request, err := http.NewRequestWithContext(ctx, method, address, nil)
		if err != nil {
			return nil, err
		}

		if c.opts.UserAgent != "" {
			request.Header.Set("User-Agent", c.opts.UserAgent)
		}

		response, err := c.client.Do(request)
		if err != nil {
			lastResponse, lastErr = nil, err
			continue
		}

		if !temporary(response.StatusCode) {
			return response, nil
		}

		// The body of an attempt that will be repeated is of no use, and
		// leaving it open would hold the connection.
		_ = response.Body.Close()

		lastResponse, lastErr = response, nil
	}

	if lastResponse != nil {
		// The last attempt's answer is what the report shows. Its body is
		// already closed, so hand back an empty one in its place.
		lastResponse.Body = io.NopCloser(strings.NewReader(""))
		return lastResponse, nil
	}

	if lastErr == nil {
		lastErr = ctx.Err()
	}

	return nil, lastErr
}

// pause waits, unless the run is cancelled first.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// visit fetches one page of the site. A failure is a fact about the page, not a
// reason to stop the crawl, so it comes back inside the Page. The parsed
// document comes back alongside it, empty when there was nothing to read.
func (c *crawl) visit(ctx context.Context, address string, depth int) (Page, document) {
	page := Page{
		URL:          address,
		Depth:        depth,
		Status:       statusFailed,
		Assets:       []Asset{},
		BrokenLinks:  []BrokenLink{},
		DiscoveredAt: now(),
	}

	var doc document

	response, err := c.request(ctx, http.MethodGet, address)
	if err != nil {
		page.Error = err.Error()
		return page, doc
	}

	defer func() { _ = response.Body.Close() }()

	page.HTTPStatus = response.StatusCode

	if response.StatusCode >= http.StatusBadRequest {
		page.Error = response.Status
		return page, doc
	}

	if base, err := url.Parse(address); err == nil && isHTML(response) {
		doc = parseDocument(base, response.Body)
	}

	page.SEO = doc.seo
	page.Status = statusOK

	return page, doc
}

// probeLink asks whether an address answers, once per address per run. HEAD is
// enough for most servers; the ones that refuse the method are asked with GET,
// because the step grades the answer and not how it was obtained.
func (c *crawl) probeLink(ctx context.Context, address string) probe {
	c.mu.Lock()
	cached, ok := c.probes[address]
	c.mu.Unlock()

	if ok {
		return cached
	}

	result := c.askOnce(ctx, http.MethodHead, address)
	if result.statusCode == http.StatusMethodNotAllowed ||
		result.statusCode == http.StatusNotImplemented {
		result = c.askOnce(ctx, http.MethodGet, address)
	}

	c.mu.Lock()
	c.probes[address] = result
	c.mu.Unlock()

	return result
}

func (c *crawl) askOnce(ctx context.Context, method, address string) probe {
	response, err := c.request(ctx, method, address)
	if err != nil {
		return probe{err: err}
	}

	_ = response.Body.Close()

	return probe{statusCode: response.StatusCode}
}

// isHTML keeps the parser away from images and downloads, which are checked but
// never read for links.
func isHTML(response *http.Response) bool {
	return strings.Contains(response.Header.Get("Content-Type"), "text/html")
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// run walks the site breadth first: one level of the tree at a time, every
// address on that level fetched by the worker pool at once. Working level by
// level is what keeps --depth meaningful, and the visited set is only touched
// between levels, so nothing has to be locked inside a level.
func (c *crawl) run(ctx context.Context, root string) []Page {
	base, err := url.Parse(root)
	if err != nil {
		page, _ := c.visit(ctx, root, 0)
		return []Page{page}
	}

	c.base = base

	var (
		pages []Page
		docs  [][]string
		refs  [][]assetRef
		seen  = map[string]struct{}{root: {}}
		level = []string{root}
	)

	for depth := 0; depth < c.opts.Depth && len(level) > 0; depth++ {
		// A cancelled run keeps what it has instead of filling the report with
		// pages that were never really fetched.
		if ctx.Err() != nil {
			break
		}

		results := c.fetchLevel(ctx, level, depth)

		var next []string

		for _, result := range results {
			pages = append(pages, result.page)
			docs = append(docs, result.doc.links)
			refs = append(refs, result.doc.assets)

			// What the crawl already learned about this address answers the
			// link check for free, so nothing is fetched twice.
			c.remember(result.page)

			for _, link := range result.doc.links {
				if !sameHost(base, link) {
					continue
				}

				if _, already := seen[link]; already {
					continue
				}

				seen[link] = struct{}{}
				next = append(next, link)
			}
		}

		level = next
	}

	c.probeUnknown(ctx, docs)
	c.fetchAssets(ctx, refs)

	for i := range pages {
		pages[i].BrokenLinks = c.brokenLinks(docs[i])
		pages[i].Assets = c.assetsFor(refs[i])
	}

	return pages
}

// remember records what a fetched page turned out to be, so a link pointing at
// it needs no separate request.
func (c *crawl) remember(page Page) {
	result := probe{statusCode: page.HTTPStatus}
	if page.HTTPStatus == 0 && page.Error != "" {
		result = probe{err: errors.New(page.Error)}
	}

	c.mu.Lock()
	c.probes[page.URL] = result
	c.mu.Unlock()
}

// probeUnknown checks, through the worker pool, every address the crawl linked
// to but never fetched itself — mostly links to other sites.
func (c *crawl) probeUnknown(ctx context.Context, docs [][]string) {
	pending := make([]string, 0)
	queued := map[string]struct{}{}

	for _, links := range docs {
		for _, link := range links {
			if _, known := c.lookup(link); known {
				continue
			}

			if _, already := queued[link]; already {
				continue
			}

			queued[link] = struct{}{}
			pending = append(pending, link)
		}
	}

	if len(pending) == 0 {
		return
	}

	queue := make(chan string)

	var wg sync.WaitGroup

	for range min(c.opts.Concurrency, len(pending)) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for address := range queue {
				c.probeLink(ctx, address)
			}
		}()
	}

	go func() {
		defer close(queue)

		for _, address := range pending {
			select {
			case <-ctx.Done():
				return
			case queue <- address:
			}
		}
	}()

	wg.Wait()
}

func (c *crawl) lookup(address string) (probe, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	result, ok := c.probes[address]

	return result, ok
}

// visited is one address after it was fetched and its links were checked.
type visited struct {
	index int
	page  Page
	doc   document
}

// fetchLevel runs one level of the walk through the worker pool and returns the
// results in the order the addresses were queued, so two runs over the same site
// produce the same report.
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

// fetchAssets asks about every distinct file the site pulls in, once each, no
// matter how many pages reference it.
func (c *crawl) fetchAssets(ctx context.Context, refs [][]assetRef) {
	pending := make([]assetRef, 0)
	queued := map[string]struct{}{}

	for _, list := range refs {
		for _, ref := range list {
			if _, already := queued[ref.url]; already {
				continue
			}

			queued[ref.url] = struct{}{}
			pending = append(pending, ref)
		}
	}

	if len(pending) == 0 {
		return
	}

	queue := make(chan assetRef)

	var wg sync.WaitGroup

	for range min(c.opts.Concurrency, len(pending)) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for ref := range queue {
				asset := c.fetchAsset(ctx, ref)

				c.mu.Lock()
				c.assets[ref.url] = asset
				c.mu.Unlock()
			}
		}()
	}

	go func() {
		defer close(queue)

		for _, ref := range pending {
			select {
			case <-ctx.Done():
				return
			case queue <- ref:
			}
		}
	}()

	wg.Wait()
}

// fetchAsset measures one file. Content-Length is believed when it is there;
// otherwise the body is read to the end and counted, because a size of zero
// would be a claim, not a measurement.
func (c *crawl) fetchAsset(ctx context.Context, ref assetRef) Asset {
	asset := Asset{URL: ref.url, Type: ref.kind}

	response, err := c.request(ctx, http.MethodGet, ref.url)
	if err != nil {
		asset.Error = err.Error()
		return asset
	}

	defer func() { _ = response.Body.Close() }()

	asset.StatusCode = response.StatusCode

	if response.StatusCode >= http.StatusBadRequest {
		asset.Error = response.Status
		return asset
	}

	if response.ContentLength >= 0 {
		asset.SizeBytes = response.ContentLength
		return asset
	}

	size, err := io.Copy(io.Discard, response.Body)
	if err != nil {
		asset.SizeBytes = 0
		asset.Error = err.Error()

		return asset
	}

	asset.SizeBytes = size

	return asset
}

// assetsFor collects one page's files from what the fetching pass measured.
func (c *crawl) assetsFor(refs []assetRef) []Asset {
	out := make([]Asset, 0, len(refs))

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, ref := range refs {
		if asset, ok := c.assets[ref.url]; ok {
			out = append(out, asset)
		}
	}

	return out
}

// brokenLinks keeps only the addresses a page points at that did not answer.
// Everything it needs was learned during the crawl or the probing pass.
func (c *crawl) brokenLinks(links []string) []BrokenLink {
	broken := make([]BrokenLink, 0)

	for _, link := range links {
		result, ok := c.lookup(link)
		if !ok || !result.broken() {
			continue
		}

		entry := BrokenLink{URL: link}
		if result.err != nil {
			entry.Error = result.err.Error()
		} else {
			entry.StatusCode = result.statusCode
			entry.Error = http.StatusText(result.statusCode)
		}

		broken = append(broken, entry)
	}

	return broken
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
		GeneratedAt: now(),
		Pages:       pages,
	}

	if opts.IndentJSON {
		return json.MarshalIndent(report, "", "  ")
	}

	return json.Marshal(report)
}
