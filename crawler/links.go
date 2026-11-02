package crawler

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"github.com/eshaan2418/trawl/urlnorm"
)

// ExtractLinks returns the normalised, de-duplicated http(s) links in an HTML document,
// honouring <base href>, rel="nofollow" on links, and <meta name="robots"> nofollow/none
// (reported as the second result: the page's links must not be followed at all).
func ExtractLinks(page *url.URL, body []byte) ([]*url.URL, bool) {
	z := html.NewTokenizer(bytes.NewReader(body))
	base := page
	seen := map[string]bool{}
	var out []*url.URL
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return out, false
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr {
				continue
			}
			attrs := map[string]string{}
			for {
				k, v, more := z.TagAttr()
				attrs[string(k)] = string(v)
				if !more {
					break
				}
			}
			switch string(name) {
			case "base":
				if b, err := url.Parse(strings.TrimSpace(attrs["href"])); err == nil && attrs["href"] != "" {
					base = page.ResolveReference(b)
				}
			case "meta":
				if strings.EqualFold(attrs["name"], "robots") {
					for _, d := range strings.Split(strings.ToLower(attrs["content"]), ",") {
						if d = strings.TrimSpace(d); d == "nofollow" || d == "none" {
							return nil, true
						}
					}
				}
			case "a", "area":
				if relNofollow(attrs["rel"]) {
					continue
				}
				if u, err := urlnorm.Normalize(base, attrs["href"]); err == nil {
					if k := urlnorm.Key(u); !seen[k] {
						seen[k] = true
						out = append(out, u)
					}
				}
			}
		}
	}
}

func relNofollow(rel string) bool {
	for _, r := range strings.Fields(strings.ToLower(rel)) {
		if r == "nofollow" {
			return true
		}
	}
	return false
}
