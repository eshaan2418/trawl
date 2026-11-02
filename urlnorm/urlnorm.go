// Package urlnorm canonicalises URLs so the same resource is crawled once.
package urlnorm

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// ErrUnsupported is returned for URLs a web crawler should not follow.
var ErrUnsupported = errors.New("urlnorm: unsupported URL")

// Normalize returns the canonical form of ref resolved against base (base may be nil):
//   - only http and https; scheme and host lower-cased; default ports removed;
//   - fragment dropped; empty path becomes "/";
//   - dot segments resolved ("/a/./b/../c" -> "/a/c"), trailing slash preserved;
//   - percent-encoding normalised (unreserved characters decoded, hex digits upper-cased).
//
// Query parameter order is preserved: reordering can change meaning on real sites.
func Normalize(base *url.URL, ref string) (*url.URL, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ErrUnsupported
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrUnsupported
	}
	if u.Host == "" || u.User != nil {
		return nil, ErrUnsupported
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimRight(host, ".") // "example.com." and "example.com.." are the same host
	if host == "" {
		return nil, ErrUnsupported // e.g. "http://." (found by FuzzIdempotent)
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" {
		u.Host = host + ":" + port
	} else {
		u.Host = host
	}
	u.Fragment = ""
	u.RawFragment = ""

	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	trailing := strings.HasSuffix(p, "/")
	p = path.Clean(p)
	if trailing && p != "/" {
		p += "/"
	}
	p = normalizeEscapes(p)
	dec, err := url.PathUnescape(p)
	if err != nil {
		return nil, err
	}
	u.Path = dec
	u.RawPath = p
	if u.RawQuery != "" {
		u.RawQuery = normalizeEscapes(escapeInvalidQuery(u.RawQuery))
	}
	u.ForceQuery = false
	return u, nil
}

// Key is the string used for de-duplication.
func Key(u *url.URL) string { return u.String() }

func normalizeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && ishex(s[i+1]) && ishex(s[i+2]) {
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if unreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteString(strings.ToUpper(s[i+1 : i+3]))
			}
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapeInvalidQuery percent-encodes bytes RFC 3986 does not allow in a query (spaces,
// controls, non-ASCII, quotes, ...). Go's parser accepts them raw, which made normalisation
// non-idempotent: "?a b" -> "?a b" but a trailing raw space was trimmed on re-parse
// (found by FuzzIdempotent). A '%' that does not start a valid escape becomes %25.
func escapeInvalidQuery(q string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '%' && i+2 < len(q) && ishex(q[i+1]) && ishex(q[i+2]) {
			b.WriteByte(c) // a valid escape; normalizeEscapes handles it
			continue
		}
		if unreserved(c) || strings.IndexByte("!$&'()*+,;=:@/?", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func ishex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

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

func unreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}
