package urlnorm

import (
	"net/url"
	"testing"
)

func TestNormalize(t *testing.T) {
	base, _ := url.Parse("http://Example.COM/a/b/c.html")
	cases := map[string]string{
		"HTTP://Example.COM:80/":        "http://example.com/",
		"https://example.com:443/x":     "https://example.com/x",
		"http://example.com:8080":       "http://example.com:8080/",
		"http://example.com/a/./b/../c": "http://example.com/a/c",
		"http://example.com/a/b/":       "http://example.com/a/b/",
		"http://example.com/%7euser":    "http://example.com/~user",
		"http://example.com/a%2fb":      "http://example.com/a%2Fb",
		"http://example.com/p?b=2&a=1":  "http://example.com/p?b=2&a=1",
		"http://example.com/p#frag":     "http://example.com/p",
		"http://example.com./x":         "http://example.com/x",
		"../d":                          "http://example.com/a/d",
		"//other.org/z":                 "http://other.org/z",
		"?q=1":                          "http://example.com/a/b/c.html?q=1",
		"http://[::1]:80/":              "http://[::1]/",
	}
	for in, want := range cases {
		u, err := Normalize(base, in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := Key(u); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestRejects(t *testing.T) {
	for _, in := range []string{"", "mailto:a@b.c", "javascript:void(0)", "ftp://x/y", "http://user:pw@x/", "http:///nohost"} {
		if _, err := Normalize(nil, in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func FuzzIdempotent(f *testing.F) {
	for _, s := range []string{"http://a/b/../c?x=%7e", "HTTPS://B:443/%41", "http://x/a//b/./"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := Normalize(nil, s)
		if err != nil {
			return
		}
		k := Key(u)
		v, err := Normalize(nil, k)
		if err != nil {
			t.Fatalf("normalised %q -> %q no longer parses: %v", s, k, err)
		}
		if Key(v) != k {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, k, Key(v))
		}
	})
}
