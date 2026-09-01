package crawler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

		return &http.Response{
			Status:     http.StatusText(status),
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
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
