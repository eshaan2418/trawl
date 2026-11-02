// Package robots implements the Robots Exclusion Protocol (RFC 9309).
//
// Matching rules, from the RFC:
//   - A crawler obeys the group whose user-agent line matches its product token
//     (case-insensitive). If several groups name the same token they are merged. If none
//     match, the "*" group applies; if there is no "*" group, everything is allowed.
//   - Within the group the longest matching rule wins; on a tie, allow wins.
//   - Patterns support "*" (any sequence) and a trailing "$" (end of path).
//   - /robots.txt itself is always allowed.
//
// Crawl-delay and Sitemap are widely used extensions that the RFC does not define; they are
// parsed too.
package robots

import (
	"bufio"
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxSize is the parsing limit the RFC requires crawlers to support (500 KiB). Content after
// it is ignored.
const MaxSize = 500 << 10

type rule struct {
	allow   bool
	pattern string // percent-encoding normalised
}

// Rules is the parsed policy for one user agent.
type Rules struct {
	rules      []rule
	crawlDelay time.Duration
	hasDelay   bool
	sitemaps   []string
}

// AllowAll is the policy when robots.txt is missing (4xx) or the agent has no group.
var AllowAll = &Rules{}

// DisallowAll is the policy when robots.txt is unreachable (5xx or network error): the RFC
// says to assume complete disallow.
var DisallowAll = &Rules{rules: []rule{{allow: false, pattern: "/"}}}

type group struct {
	agents []string
	rules  []rule
	delay  time.Duration
	hasDly bool
}

// Parse parses robots.txt content for the crawler whose product token is `agent`
// (for example "trawl"; full User-Agent strings are reduced to their first token).
func Parse(body []byte, agent string) *Rules {
	if len(body) > MaxSize {
		body = body[:MaxSize]
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	token := productToken(agent)

	var groups []*group
	var cur *group
	lastWasAgent := false
	var sitemaps []string

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), MaxSize)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent", "useragent", "user agent":
			if cur == nil || !lastWasAgent {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(productToken(val)))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil {
				continue // rules before any user-agent line are ignored
			}
			if val == "" {
				continue // an empty Allow/Disallow value adds no rule ("Disallow:" allows all)
			}
			cur.rules = append(cur.rules, rule{allow: key == "allow", pattern: normalizePattern(val)})
		case "crawl-delay":
			lastWasAgent = false
			if cur == nil {
				continue
			}
			if secs, err := strconv.ParseFloat(val, 64); err == nil && secs >= 0 && secs < 86400 {
				cur.delay = time.Duration(secs * float64(time.Second))
				cur.hasDly = true
			}
		case "sitemap":
			// Sitemap lines are global, not part of a group, and do not end one.
			sitemaps = append(sitemaps, val)
		default:
			lastWasAgent = false
		}
	}

	r := &Rules{sitemaps: sitemaps}
	matched := false
	for _, want := range []string{strings.ToLower(token), "*"} {
		for _, g := range groups {
			for _, a := range g.agents {
				if a == want {
					r.rules = append(r.rules, g.rules...)
					if g.hasDly && !r.hasDelay {
						r.crawlDelay, r.hasDelay = g.delay, true
					}
					matched = true
					break
				}
			}
		}
		if matched {
			break
		}
	}
	return r
}

// productToken reduces "Mozilla/5.0 (compatible; trawl/1.0)" style values to the leading
// token of letters, digits, '-' and '_' ("Mozilla"). For our own agent we pass the bare token.
func productToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "*" {
		return s
	}
	end := 0
	for end < len(s) {
		c := s[end]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			end++
			continue
		}
		break
	}
	return s[:end]
}

// Allowed reports whether a URL path (with optional query) may be fetched.
func (r *Rules) Allowed(pathAndQuery string) bool {
	if pathAndQuery == "" {
		pathAndQuery = "/"
	}
	if pathAndQuery == "/robots.txt" {
		return true
	}
	p := normalizePattern(pathAndQuery)
	bestLen := -1
	allowed := true
	for _, ru := range r.rules {
		if !match(ru.pattern, p) {
			continue
		}
		l := len(ru.pattern)
		if l > bestLen || (l == bestLen && ru.allow) {
			bestLen = l
			allowed = ru.allow
		}
	}
	return allowed
}

// AllowedURL is Allowed for a full URL.
func (r *Rules) AllowedURL(u *url.URL) bool {
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return r.Allowed(p)
}

// CrawlDelay returns the Crawl-delay for the matched group, if one was given.
func (r *Rules) CrawlDelay() (time.Duration, bool) { return r.crawlDelay, r.hasDelay }

// Sitemaps lists Sitemap URLs declared anywhere in the file.
func (r *Rules) Sitemaps() []string { return r.sitemaps }

// match implements RFC 9309 pattern matching: '*' matches any sequence, a trailing '$'
// anchors at the end, everything else is a literal prefix match. Linear-time two-pointer
// algorithm with backtracking to the last '*' (no recursion, no regexp).
func match(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	p, s := 0, 0
	star, mark := -1, 0
	for s < len(path) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, s
			p++
		case p < len(pattern) && pattern[p] == path[s]:
			p++
			s++
		case p == len(pattern) && !anchored:
			return true // prefix match complete
		case star >= 0:
			p = star + 1
			mark++
			s = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// normalizePattern makes percent-encoding comparable: unreserved characters that are
// percent-encoded are decoded, other escapes are upper-cased, and non-ASCII bytes are encoded.
func normalizePattern(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if isUnreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteByte(upper(s[i+1]))
				b.WriteByte(upper(s[i+2]))
			}
			i += 2
		case c >= 0x80:
			b.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(c), 16)))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 32
	}
	return c
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}
