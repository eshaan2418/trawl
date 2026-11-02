package robots

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRFC9309Examples(t *testing.T) {
	// Group selection, merging, and longest-match examples in the spirit of RFC 9309 §2.2.
	body := `
User-Agent: *
Disallow: *.gif$
Disallow: /example/
Allow: /publications/

User-Agent: foobot
Disallow:/
Allow:/example/page.html
Allow:/example/allowed.gif

User-Agent: barbot
User-Agent: bazbot
Disallow: /example/page.html

User-Agent: quxbot
`
	cases := []struct {
		agent, path string
		want        bool
	}{
		{"foobot", "/", false},
		{"foobot", "/example/page.html", true},
		{"foobot", "/example/allowed.gif", true},
		{"foobot", "/example/other.gif", false},
		{"barbot", "/example/page.html", false},
		{"bazbot", "/example/page.html", false},
		{"barbot", "/example/index.html", true}, // barbot's group has no rule for it
		{"quxbot", "/anything.gif", true},       // empty group: allow all, no fallback to *
		{"otherbot", "/example/x", false},       // falls back to *
		{"otherbot", "/image.gif", false},
		{"otherbot", "/image.gif?x=1", true}, // $ anchors at the end of path+query
		{"otherbot", "/publications/2024", true},
		{"FooBot", "/example/page.html", true}, // agent match is case-insensitive
	}
	for _, c := range cases {
		if got := Parse([]byte(body), c.agent).Allowed(c.path); got != c.want {
			t.Errorf("%s %s: got %v want %v", c.agent, c.path, got, c.want)
		}
	}
}

func TestLongestMatchAndTieGoesToAllow(t *testing.T) {
	r := Parse([]byte("user-agent: *\nallow: /page\ndisallow: /*.html\n"), "x")
	if r.Allowed("/page.html") { // "/*.html" (7) is longer than "/page" (5)
		t.Error("longer disallow should win")
	}
	r = Parse([]byte("user-agent: *\nallow: /folder\ndisallow: /folder\n"), "x")
	if !r.Allowed("/folder/page") {
		t.Error("equal length: allow wins")
	}
}

func TestEdgeCases(t *testing.T) {
	cases := []struct {
		body, path string
		want       bool
	}{
		{"", "/x", true},
		{"Disallow: /\n", "/x", true},                         // rule outside any group is ignored
		{"user-agent: *\ndisallow:\n", "/x", true},            // empty disallow = allow all
		{"user-agent: *\nallow:\ndisallow: /\n", "/x", false}, // empty allow adds no rule
		{"user-agent: *\ndisallow: /\n", "/robots.txt", true}, // robots.txt always allowed
		{"\xef\xbb\xbfuser-agent: *\ndisallow: /a\n", "/a", false},
		{"user-agent: *  # comment\ndisallow: /a # trailing\n", "/a", false},
		{"user-agent: *\ndisallow: /%7Ejoe/\n", "/~joe/x", false}, // %7E is unreserved: decoded
		{"user-agent: *\ndisallow: /a%3cb\n", "/a%3Cb", false},    // escapes compare case-insensitively
		{"user-agent: *\ndisallow: /fish*.php\n", "/fishheads/catfish.php?p", false},
		{"user-agent: *\ndisallow: /*.php$\n", "/filename.php/", true},
		{"USER-AGENT: *\nDISALLOW: /A\n", "/A", false},                              // field names are case-insensitive
		{"user-agent: *\ndisallow: /a\nuser-agent: *\ndisallow: /b\n", "/b", false}, // merged
	}
	for _, c := range cases {
		if got := Parse([]byte(c.body), "trawl").Allowed(c.path); got != c.want {
			t.Errorf("%q %s: got %v want %v", c.body, c.path, got, c.want)
		}
	}
}

func TestCrawlDelayAndSitemaps(t *testing.T) {
	r := Parse([]byte("Sitemap: https://a/s.xml\nuser-agent: trawl\ncrawl-delay: 2.5\nuser-agent: *\ncrawl-delay: 9\n"), "trawl")
	if d, ok := r.CrawlDelay(); !ok || d != 2500*time.Millisecond {
		t.Errorf("crawl-delay %v %v", d, ok)
	}
	if s := r.Sitemaps(); len(s) != 1 || s[0] != "https://a/s.xml" {
		t.Errorf("sitemaps %v", s)
	}
	if _, ok := Parse([]byte("user-agent: *\ncrawl-delay: -1\n"), "x").CrawlDelay(); ok {
		t.Error("negative crawl-delay must be ignored")
	}
}

func TestDisallowAllAndAllowAll(t *testing.T) {
	if DisallowAll.Allowed("/a") || !DisallowAll.Allowed("/robots.txt") || !AllowAll.Allowed("/a") {
		t.Fatal("sentinel policies")
	}
}

func TestSizeLimit(t *testing.T) {
	big := "user-agent: *\n" + strings.Repeat("# padding padding padding\n", MaxSize/26+10) + "disallow: /late\n"
	if !Parse([]byte(big), "x").Allowed("/late") {
		t.Error("rules beyond 500 KiB must be ignored")
	}
}

// refMatch translates a pattern to a regular expression: the reference for fuzzing match().
func refMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	var b strings.Builder
	b.WriteString("^")
	for _, part := range strings.Split(pattern, "*") {
		b.WriteString(regexp.QuoteMeta(part))
		b.WriteString(".*")
	}
	re := strings.TrimSuffix(b.String(), ".*")
	if anchored {
		re += "$"
	}
	return regexp.MustCompile("(?s)" + re).MatchString(path)
}

func FuzzMatchAgainstRegexp(f *testing.F) {
	for _, s := range [][2]string{{"/a*b$", "/axxb"}, {"/fish*.php", "/fish/x.php?y"}, {"*", ""}, {"/$", "/"}, {"**a", "ba"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		if len(pattern) > 64 || len(path) > 128 || !ascii(pattern) || !ascii(path) {
			return // patterns and paths are percent-encoded to ASCII before matching
		}
		if got, want := match(pattern, path), refMatch(pattern, path); got != want {
			t.Fatalf("match(%q, %q) = %v, regexp says %v", pattern, path, got, want)
		}
	})
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("user-agent: *\ndisallow: /a\nallow: /a/b\ncrawl-delay: 1\n"), "/a/b/c")
	f.Fuzz(func(t *testing.T, body []byte, path string) {
		r := Parse(body, "trawl")
		a := r.Allowed(path)
		if a != Parse(body, "trawl").Allowed(path) {
			t.Fatal("non-deterministic")
		}
		if !r.Allowed("/robots.txt") {
			t.Fatal("/robots.txt must always be allowed")
		}
	})
}
