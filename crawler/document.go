package crawler

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// SEO is what a page says about itself to a search engine.
type SEO struct {
	HasTitle       bool   `json:"has_title"`
	Title          string `json:"title"`
	HasDescription bool   `json:"has_description"`
	Description    string `json:"description"`
	HasH1          bool   `json:"has_h1"`
}

// document is what one fetched page tells the crawler: the addresses it points
// at and the tags a search engine reads.
type document struct {
	links []string
	seo   SEO
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
		if node.Type == html.ElementNode {
			switch node.Data {
			case "a":
				if href, ok := attr(node, "href"); ok {
					if resolved, ok := resolve(base, href); ok {
						if _, already := seen[resolved]; !already {
							seen[resolved] = struct{}{}
							doc.links = append(doc.links, resolved)
						}
					}
				}
			case "title":
				// Only the first title counts; a second one is not a page title.
				if !doc.seo.HasTitle {
					doc.seo.HasTitle = true
					doc.seo.Title = text(node)
				}
			case "meta":
				if name, ok := attr(node, "name"); ok && strings.EqualFold(name, "description") {
					if content, ok := attr(node, "content"); ok && !doc.seo.HasDescription {
						doc.seo.HasDescription = true
						doc.seo.Description = clean(content)
					}
				}
			case "h1":
				doc.seo.HasH1 = true
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return doc
}

// text collects the readable text inside a node. The parser has already turned
// entities such as &amp; back into their characters, so nothing else has to.
func text(node *html.Node) string {
	var b strings.Builder

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}

		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(node)

	return clean(b.String())
}

// clean squeezes the whitespace a human would not have typed out of a value.
func clean(value string) string {
	return strings.Join(strings.Fields(value), " ")
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
