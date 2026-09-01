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

// assetRef is a file a page pulls in — a picture, a script, a stylesheet.
type assetRef struct {
	url  string
	kind string
}

// document is what one fetched page tells the crawler: the addresses it points
// at, the files it pulls in, and the tags a search engine reads.
type document struct {
	links  []string
	assets []assetRef
	seo    SEO
}

// parseDocument reads an HTML body and resolves every href it finds against
// the page's own address, so a relative link becomes something fetchable.
func parseDocument(base *url.URL, body io.Reader) document {
	root, err := html.Parse(body)
	if err != nil {
		return document{}
	}

	if declared := baseHref(base, root); declared != nil {
		base = declared
	}

	var (
		doc        document
		seen       = map[string]struct{}{}
		seenAssets = map[string]struct{}{}
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
			case "img":
				addAsset(base, node, "src", assetImage, &doc, seenAssets)
			case "script":
				addAsset(base, node, "src", assetScript, &doc, seenAssets)
			case "link":
				if rel, ok := attr(node, "rel"); ok && hasToken(rel, "stylesheet") {
					addAsset(base, node, "href", assetStyle, &doc, seenAssets)
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

// baseHref finds the address the page itself declares as the starting point for
// its relative links. HTML says a <base href> wins over the page's own address,
// and a page served from one path but written for another relies on it.
func baseHref(pageURL *url.URL, root *html.Node) *url.URL {
	var found *url.URL

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != nil {
			return
		}

		if node.Type == html.ElementNode && node.Data == "base" {
			if href, ok := attr(node, "href"); ok {
				if parsed, err := url.Parse(strings.TrimSpace(href)); err == nil {
					found = pageURL.ResolveReference(parsed)
				}
			}

			return
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(root)

	return found
}

// asset kinds, as the report spells them.
const (
	assetImage  = "image"
	assetScript = "script"
	assetStyle  = "style"
)

// addAsset records a file the page pulls in, once per address per page.
func addAsset(base *url.URL, node *html.Node, attribute, kind string, doc *document, seen map[string]struct{}) {
	value, ok := attr(node, attribute)
	if !ok {
		return
	}

	resolved, ok := resolve(base, value)
	if !ok {
		return
	}

	if _, already := seen[resolved]; already {
		return
	}

	seen[resolved] = struct{}{}
	doc.assets = append(doc.assets, assetRef{url: resolved, kind: kind})
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

// hasToken says whether a space-separated attribute such as rel="preload
// stylesheet" carries the word looked for.
func hasToken(value, token string) bool {
	for _, word := range strings.Fields(value) {
		if strings.EqualFold(word, token) {
			return true
		}
	}

	return false
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

	return hostKey(parsed) == hostKey(base)
}

// hostKey is the host as the site is identified by it. A port written out in
// full — example.test:80 over http — names the same server as example.test, and
// a crawl that treats them as two sites walks half of it as if it were foreign.
func hostKey(u *url.URL) string {
	host := strings.ToLower(u.Hostname())

	port := u.Port()
	switch {
	case port == "":
	case u.Scheme == "http" && port == "80":
	case u.Scheme == "https" && port == "443":
	default:
		host += ":" + port
	}

	return host
}
