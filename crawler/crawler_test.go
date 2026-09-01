package crawler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code/crawler"
)

// roundTripFunc lets a test stand in for the network without a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func clientReturning(status int, body string, calls *[]*http.Request) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls != nil {
			*calls = append(*calls, r)
		}

		header := make(http.Header)
		header.Set("Content-Type", "text/html; charset=utf-8")

		return &http.Response{
			Status:     http.StatusText(status),
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     header,
		}, nil
	})}
}

// site serves a small fixed set of pages, so a walk can be checked without a
// server. A path that is not in the map answers 404.
func site(pages map[string]string, requests *sync.Map) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if requests != nil {
			count, _ := requests.LoadOrStore(r.URL.String(), new(int64))
			atomic.AddInt64(count.(*int64), 1)
		}

		header := make(http.Header)
		header.Set("Content-Type", "text/html; charset=utf-8")

		body, ok := pages[r.URL.String()]
		if !ok {
			return &http.Response{
				Status:     http.StatusText(http.StatusNotFound),
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     header,
			}, nil
		}

		return &http.Response{
			Status:     http.StatusText(http.StatusOK),
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     header,
		}, nil
	})}
}

func urls(report crawler.Report) []string {
	out := make([]string, 0, len(report.Pages))
	for _, page := range report.Pages {
		out = append(out, page.URL)
	}

	return out
}

func clientFailing(err error, calls *int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls != nil {
			*calls++
		}

		return nil, err
	})}
}

func analyze(t *testing.T, opts crawler.Options) crawler.Report {
	t.Helper()

	raw, err := crawler.Analyze(context.Background(), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	var report crawler.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	return report
}

func TestAnalyzeReportsTheRootPage(t *testing.T) {
	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		Depth:      1,
		HTTPClient: clientReturning(http.StatusOK, "<html></html>", nil),
	})

	if report.RootURL != "https://example.com" || report.Depth != 1 {
		t.Fatalf("report = %+v", report)
	}

	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		t.Fatalf("generated_at = %q, want RFC3339: %v", report.GeneratedAt, err)
	}

	if len(report.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(report.Pages))
	}

	page := report.Pages[0]
	if page.URL != "https://example.com" || page.Depth != 0 ||
		page.HTTPStatus != http.StatusOK || page.Status != "ok" || page.Error != "" {
		t.Fatalf("page = %+v", page)
	}
}

func TestAnalyzeSendsTheCustomUserAgent(t *testing.T) {
	var calls []*http.Request

	analyze(t, crawler.Options{
		URL:        "https://example.com",
		UserAgent:  "hexlet-go-crawler/test",
		HTTPClient: clientReturning(http.StatusOK, "", &calls),
	})

	if len(calls) != 1 {
		t.Fatalf("made %d requests, want 1", len(calls))
	}

	if agent := calls[0].Header.Get("User-Agent"); agent != "hexlet-go-crawler/test" {
		t.Fatalf("User-Agent = %q", agent)
	}
}

func TestAnalyzeKeepsAnErrorStatusOnThePage(t *testing.T) {
	report := analyze(t, crawler.Options{
		URL:        "https://example.com/missing",
		HTTPClient: clientReturning(http.StatusNotFound, "", nil),
	})

	page := report.Pages[0]
	if page.HTTPStatus != http.StatusNotFound || page.Status != "failed" || page.Error == "" {
		t.Fatalf("page = %+v", page)
	}
}

func TestAnalyzeSurvivesANetworkFailure(t *testing.T) {
	calls := 0

	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		Retries:    2,
		HTTPClient: clientFailing(errors.New("connection refused"), &calls),
	})

	if calls != 3 {
		t.Fatalf("made %d attempts, want 3 (the first plus two retries)", calls)
	}

	page := report.Pages[0]
	if page.Status != "failed" || page.HTTPStatus != 0 || page.Error == "" {
		t.Fatalf("page = %+v", page)
	}
}

func TestAnalyzeWithoutAURLIsAnError(t *testing.T) {
	_, err := crawler.Analyze(context.Background(), crawler.Options{})
	if !errors.Is(err, crawler.ErrNoURL) {
		t.Fatalf("err = %v, want ErrNoURL", err)
	}
}

func TestAnalyzeCanIndentTheReport(t *testing.T) {
	raw, err := crawler.Analyze(context.Background(), crawler.Options{
		URL:        "https://example.com",
		IndentJSON: true,
		HTTPClient: clientReturning(http.StatusOK, "", nil),
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if !strings.Contains(string(raw), "\n  \"root_url\"") {
		t.Fatalf("report is not indented: %s", raw)
	}
}

func TestAnalyzeFollowsLinksWithinTheSite(t *testing.T) {
	pages := map[string]string{
		"https://example.com/":     `<a href="/one">1</a> <a href="/two">2</a> <a href="https://other.test/x">out</a>`,
		"https://example.com/one":  `<a href="/deep">deep</a>`,
		"https://example.com/two":  ``,
		"https://example.com/deep": ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      3,
		HTTPClient: site(pages, nil),
	})

	got := urls(report)
	want := []string{
		"https://example.com/",
		"https://example.com/one",
		"https://example.com/two",
		"https://example.com/deep",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("visited %v, want %v", got, want)
	}

	for _, page := range report.Pages {
		if page.URL == "https://example.com/one" && page.Depth != 1 {
			t.Fatalf("depth of /one = %d, want 1", page.Depth)
		}
	}
}

func TestAnalyzeStopsAtTheRequestedDepth(t *testing.T) {
	pages := map[string]string{
		"https://example.com/":    `<a href="/one">1</a>`,
		"https://example.com/one": `<a href="/two">2</a>`,
		"https://example.com/two": ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      2,
		HTTPClient: site(pages, nil),
	})

	if got := urls(report); !slices.Equal(got, []string{"https://example.com/", "https://example.com/one"}) {
		t.Fatalf("visited %v, want the root and one level below it", got)
	}
}

func TestAnalyzeDoesNotFollowLinksOffTheSite(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<a href="https://other.test/a">out</a>`,
		"https://other.test/a": `<a href="https://other.test/b">deeper</a>`,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      5,
		HTTPClient: site(pages, nil),
	})

	for _, page := range report.Pages {
		if page.URL == "https://other.test/b" {
			t.Fatal("the crawl descended into another host")
		}
	}
}

func TestAnalyzeVisitsEachAddressOnce(t *testing.T) {
	var requests sync.Map

	pages := map[string]string{
		"https://example.com/":    `<a href="/one">1</a><a href="/one#top">again</a><a href="/two">2</a>`,
		"https://example.com/one": `<a href="/">home</a><a href="/two">2</a>`,
		"https://example.com/two": `<a href="/one">1</a>`,
	}

	analyze(t, crawler.Options{
		URL:         "https://example.com/",
		Depth:       5,
		Concurrency: 4,
		HTTPClient:  site(pages, &requests),
	})

	requests.Range(func(key, value any) bool {
		if count := atomic.LoadInt64(value.(*int64)); count != 1 {
			t.Errorf("%v fetched %d times, want once", key, count)
		}

		return true
	})
}

func TestAnalyzeKeepsOnlyTheBrokenLinks(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `
			<a href="/works">fine</a>
			<a href="/missing">gone</a>
			<a href="mailto:hi@example.com">write</a>
			<a href="javascript:void(0)">nothing</a>
			<a href="">empty</a>
			<a href="#top">anchor</a>`,
		"https://example.com/works": ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      1,
		HTTPClient: site(pages, nil),
	})

	broken := report.Pages[0].BrokenLinks
	if len(broken) != 1 {
		t.Fatalf("broken links = %+v, want exactly the missing one", broken)
	}

	if broken[0].URL != "https://example.com/missing" || broken[0].StatusCode != http.StatusNotFound {
		t.Fatalf("broken link = %+v", broken[0])
	}
}

func TestAnalyzeRecordsANetworkErrorAsABrokenLink(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://example.com/" {
			header := make(http.Header)
			header.Set("Content-Type", "text/html")

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`<a href="https://cdn.example.test/app.js">js</a>`)),
				Header:     header,
			}, nil
		}

		return nil, errors.New("dial tcp: lookup cdn.example.test: no such host")
	})}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      1,
		HTTPClient: client,
	})

	broken := report.Pages[0].BrokenLinks
	if len(broken) != 1 || broken[0].Error == "" || broken[0].StatusCode != 0 {
		t.Fatalf("broken links = %+v", broken)
	}
}

func TestAnalyzeFillsDiscoveredAt(t *testing.T) {
	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, "", nil),
	})

	if _, err := time.Parse(time.RFC3339, report.Pages[0].DiscoveredAt); err != nil {
		t.Fatalf("discovered_at = %q: %v", report.Pages[0].DiscoveredAt, err)
	}
}

// clientTimingOut never answers: it waits for the request's own deadline and
// then reports it, the way a real client does on a slow host.
func clientTimingOut() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
}

func TestAnalyzeRecordsAServerError(t *testing.T) {
	report := analyze(t, crawler.Options{
		URL:        "https://example.com/boom",
		HTTPClient: clientReturning(http.StatusInternalServerError, "", nil),
	})

	page := report.Pages[0]
	if page.HTTPStatus != http.StatusInternalServerError || page.Status != "failed" || page.Error == "" {
		t.Fatalf("page = %+v", page)
	}
}

func TestAnalyzeRecordsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	raw, err := crawler.Analyze(ctx, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientTimingOut(),
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	var report crawler.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	page := report.Pages[0]
	if page.Status != "failed" || page.Error == "" {
		t.Fatalf("page = %+v", page)
	}

	if !strings.Contains(page.Error, "deadline exceeded") && !strings.Contains(page.Error, "context") {
		t.Fatalf("error = %q, want it to name the timeout", page.Error)
	}
}

func TestAnalyzeNeverTouchesTheDefaultClient(t *testing.T) {
	// The whole point of passing a client in: if the crawler ever reached for
	// the package-level one, this stub would not see the request at all.
	var calls []*http.Request

	analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, "", &calls),
	})

	if len(calls) != 1 {
		t.Fatalf("the injected client saw %d requests, want 1", len(calls))
	}
}

func TestAnalyzeReadsTheSEOTags(t *testing.T) {
	body := `<html><head>
		<title>Example Test</title>
		<meta name="description" content="A page about tea &amp; coffee">
	</head><body><h1>Hello</h1></body></html>`

	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, body, nil),
	})

	seo := report.Pages[0].SEO
	if !seo.HasTitle || seo.Title != "Example Test" {
		t.Fatalf("title = %q (has: %v)", seo.Title, seo.HasTitle)
	}

	if !seo.HasDescription || seo.Description != "A page about tea & coffee" {
		t.Fatalf("description = %q (has: %v)", seo.Description, seo.HasDescription)
	}

	if !seo.HasH1 {
		t.Fatal("h1 was not noticed")
	}
}

func TestAnalyzeReportsMissingSEOTags(t *testing.T) {
	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, `<html><body><p>bare</p></body></html>`, nil),
	})

	seo := report.Pages[0].SEO
	if seo.HasTitle || seo.Title != "" || seo.HasDescription || seo.Description != "" || seo.HasH1 {
		t.Fatalf("seo = %+v, want every flag false and every string empty", seo)
	}
}

func TestAnalyzeDecodesHTMLEntitiesInTheTitle(t *testing.T) {
	body := `<html><head><title>Tea &amp; Coffee &lt;shop&gt;</title></head><body></body></html>`

	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, body, nil),
	})

	if title := report.Pages[0].SEO.Title; title != "Tea & Coffee <shop>" {
		t.Fatalf("title = %q", title)
	}
}

func TestAnalyzeTrimsWhitespaceInSEOText(t *testing.T) {
	body := "<html><head><title>\n   Spaced   out\n  </title></head><body></body></html>"

	report := analyze(t, crawler.Options{
		URL:        "https://example.com",
		HTTPClient: clientReturning(http.StatusOK, body, nil),
	})

	if title := report.Pages[0].SEO.Title; title != "Spaced out" {
		t.Fatalf("title = %q", title)
	}
}

func TestAnalyzeAtDepthOneVisitsOnlyTheStartPage(t *testing.T) {
	pages := map[string]string{
		"https://example.com/":    `<a href="/one">1</a><a href="/two">2</a>`,
		"https://example.com/one": ``,
		"https://example.com/two": ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      1,
		HTTPClient: site(pages, nil),
	})

	if got := urls(report); !slices.Equal(got, []string{"https://example.com/"}) {
		t.Fatalf("visited %v, want only the start page", got)
	}
}

func TestAnalyzeCountsDepthAsHopsFromTheStart(t *testing.T) {
	pages := map[string]string{
		"https://example.com/":         `<a href="/one">1</a><a href="https://other.test/x">out</a>`,
		"https://example.com/one":      `<a href="/one/deep">deep</a>`,
		"https://example.com/one/deep": ``,
		"https://other.test/x":         ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      3,
		HTTPClient: site(pages, nil),
	})

	want := map[string]int{
		"https://example.com/":         0,
		"https://example.com/one":      1,
		"https://example.com/one/deep": 2,
	}

	if len(report.Pages) != len(want) {
		t.Fatalf("visited %v, want %d pages", urls(report), len(want))
	}

	for _, page := range report.Pages {
		if page.Depth != want[page.URL] {
			t.Fatalf("%s has depth %d, want %d", page.URL, page.Depth, want[page.URL])
		}
	}
}

func TestAnalyzeListsADuplicatedLinkOnce(t *testing.T) {
	pages := map[string]string{
		"https://example.com/":    `<a href="/one">a</a><a href="/one">b</a><a href="/one#x">c</a>`,
		"https://example.com/one": ``,
	}

	report := analyze(t, crawler.Options{
		URL:        "https://example.com/",
		Depth:      3,
		HTTPClient: site(pages, nil),
	})

	if got := urls(report); !slices.Equal(got, []string{"https://example.com/", "https://example.com/one"}) {
		t.Fatalf("visited %v, want each address once", got)
	}
}

func TestAnalyzeReturnsValidJSONWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	raw, err := crawler.Analyze(ctx, crawler.Options{
		URL:        "https://example.com/",
		Depth:      5,
		HTTPClient: site(map[string]string{"https://example.com/": `<a href="/one">1</a>`}, nil),
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	var report crawler.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	if report.RootURL != "https://example.com/" {
		t.Fatalf("report = %+v", report)
	}
}

// linkedSite builds a page linking to n others, so a crawl makes a known
// number of requests.
func linkedSite(n int) map[string]string {
	pages := map[string]string{}

	var links strings.Builder

	for i := 1; i <= n; i++ {
		target := fmt.Sprintf("https://example.com/%d", i)
		fmt.Fprintf(&links, `<a href="%s">%d</a>`, target, i)
		pages[target] = ""
	}

	pages["https://example.com/"] = links.String()

	return pages
}

func TestAnalyzeSpacesRequestsByTheDelay(t *testing.T) {
	const (
		pages = 4
		delay = 60 * time.Millisecond
	)

	start := time.Now()

	analyze(t, crawler.Options{
		URL:         "https://example.com/",
		Depth:       2,
		Concurrency: 4,
		Delay:       delay,
		HTTPClient:  site(linkedSite(pages), nil),
	})

	// Five addresses are fetched (the root and its four links), so at least
	// four gaps must have been waited out even though four workers were free.
	if elapsed := time.Since(start); elapsed < time.Duration(pages)*delay {
		t.Fatalf("the crawl took %v, want at least %v", elapsed, time.Duration(pages)*delay)
	}
}

func TestAnalyzeIsNotSlowedDownWithoutALimit(t *testing.T) {
	start := time.Now()

	analyze(t, crawler.Options{
		URL:         "https://example.com/",
		Depth:       2,
		Concurrency: 4,
		HTTPClient:  site(linkedSite(6), nil),
	})

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an unlimited crawl took %v", elapsed)
	}
}

func TestRPSWinsOverDelay(t *testing.T) {
	const pages = 3

	start := time.Now()

	analyze(t, crawler.Options{
		URL:         "https://example.com/",
		Depth:       2,
		Concurrency: 4,
		Delay:       2 * time.Second, // would make this test take seconds
		RPS:         20,              // 50ms apart wins
		HTTPClient:  site(linkedSite(pages), nil),
	})

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the crawl took %v — the delay was used instead of the rps", elapsed)
	}
}

func TestCancellingStopsTheWaitAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()

	if _, err := crawler.Analyze(ctx, crawler.Options{
		URL:         "https://example.com/",
		Depth:       2,
		Concurrency: 1,
		Delay:       10 * time.Second,
		HTTPClient:  site(linkedSite(5), nil),
	}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancelling left the crawl waiting for %v", elapsed)
	}
}
