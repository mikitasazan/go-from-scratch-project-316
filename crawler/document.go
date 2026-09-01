package crawler

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// document is what one fetched page tells the crawler about the rest of the
// site: the addresses it points at.
type document struct {
	links []string
}

// parseDocument reads an HTML body and resolves every href it finds against
// the page's own address, so a relative link becomes something fetchable.
func parseDocument(base *url.URL, body io.Reader) document {
	root, err := html.Parse(body)
	if err != nil {
		return document{}
	}

	var (
		doc  document
		seen = map[string]struct{}{}
	)

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "a" {
			if href, ok := attr(node, "href"); ok {
				if resolved, ok := resolve(base, href); ok {
					if _, already := seen[resolved]; !already {
						seen[resolved] = struct{}{}
						doc.links = append(doc.links, resolved)
					}
				}
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return doc
}

func attr(node *html.Node, name string) (string, bool) {
	for _, a := range node.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}

	return "", false
}

// resolve turns one href into an absolute http(s) address, dropping the
// fragment so `/page` and `/page#top` are not crawled twice. Anything that is
// not a web address — mailto:, tel:, javascript: — is skipped.
func resolve(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}

	parsed, err := url.Parse(href)
	if err != nil {
		return "", false
	}

	absolute := base.ResolveReference(parsed)
	if absolute.Scheme != "http" && absolute.Scheme != "https" {
		return "", false
	}

	absolute.Fragment = ""

	return absolute.String(), true
}

// sameHost says whether an address belongs to the site being crawled. Only
// those are descended into; anything else is checked and left alone.
func sameHost(base *url.URL, raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Host, base.Host)
}
