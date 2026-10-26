package richtext

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestRenderEscapesAndDropsUnsafe(t *testing.T) {
	doc := `{"type":"doc","content":[
	 {"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Hello <World>"}]},
	 {"type":"paragraph","content":[
	   {"type":"text","text":"click","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]},
	   {"type":"text","text":" safe","marks":[{"type":"link","attrs":{"href":"https://example.com/?a=1&b=\"2\"","target":"_blank"}},{"type":"bold"}]},
	   {"type":"text","text":"x","marks":[{"type":"evil","attrs":{"onclick":"x"}}]}
	 ]},
	 {"type":"script","content":[{"type":"text","text":"alert(1)"}]},
	 {"type":"codeBlock","attrs":{"language":"go\" onmouseover=\"x"},"content":[{"type":"text","text":"<b>x</b>","marks":[{"type":"bold"}]}]}
	]}`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out := string(HTML(n, Options{AnchorHeadings: true}))
	for _, bad := range []string{"javascript:", "<World>", "script", "onclick", "onmouseover", "<b>"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{`<h2 id="hello-world">Hello &lt;World&gt;</h2>`, `rel="noopener noreferrer"`, `<strong> safe</strong>`, `&lt;b&gt;x&lt;/b&gt;`, "click"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLimits(t *testing.T) {
	deep := strings.Repeat(`{"type":"blockquote","content":[`, 40) + `{"type":"paragraph"}` + strings.Repeat(`]}`, 40)
	if _, err := Parse([]byte(`{"type":"doc","content":[` + deep + `]}`)); err == nil {
		t.Fatal("expected depth error")
	}
	if _, err := Parse([]byte(`{"type":"paragraph"}`)); err == nil {
		t.Fatal("expected root error")
	}
}

func TestSafeURL(t *testing.T) {
	ok := []string{"https://a.b/c", "/relative", "#anchor", "mailto:a@b.c", "tel:+44"}
	bad := []string{"javascript:x", "JaVaScRiPt:x", "data:text/html,x", "//evil.com", "vbscript:x", " java\tscript:x", ""}
	for _, u := range ok {
		if _, k := SafeURL(u); !k {
			t.Errorf("rejected %q", u)
		}
	}
	for _, u := range bad {
		if _, k := SafeURL(u); k {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{"Hello, World!": "hello-world", "Café & Crème": "cafe-and-creme", "  --x--  ": "x", "Don't Stop": "dont-stop"}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzRender(f *testing.F) {
	f.Add(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"<x>","marks":[{"type":"link","attrs":{"href":"javascript:1"}}]}]}]}`)
	f.Add(`{"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"\"><svg"},"content":[{"type":"text","text":"a"}]}]}`)
	// A fragment that merely contains "javascript:" is a same-page link, not script.
	f.Add(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"<x>","marks":[{"type":"link","attrs":{"href":"#000000000javascript:0"}}]}]}]}`)
	f.Fuzz(func(t *testing.T, in string) {
		n, err := Parse([]byte(in))
		if err != nil {
			return
		}
		out := strings.ToLower(string(HTML(n, Options{AnchorHeadings: true})))
		// Judge each URL as a browser would: decode the attribute, then look
		// at the scheme. (A plain substring check flags "#…javascript:…".)
		for _, m := range urlAttrRe.FindAllStringSubmatch(out, -1) {
			v := strings.TrimSpace(html.UnescapeString(m[1]))
			u, err := url.Parse(v)
			if err != nil || strings.HasPrefix(v, "//") || !allowedSchemes[u.Scheme] {
				t.Fatalf("unsafe url %q in %q", v, out)
			}
		}
		for _, m := range tagRe.FindAllStringSubmatch(out, -1) {
			if !allowedTags[m[1]] {
				t.Fatalf("unexpected tag <%s> in %q for input %q", m[1], out, in)
			}
		}
		for _, m := range attrRe.FindAllStringSubmatch(out, -1) {
			if !allowedAttrs[m[1]] {
				t.Fatalf("unexpected attribute %s in %q", m[1], out)
			}
		}
	})
}

var (
	tagRe          = regexp.MustCompile(`</?([a-z0-9]+)`)
	attrRe         = regexp.MustCompile(`<[a-z0-9]+[^>]*?\s([a-z-]+)=`)
	urlAttrRe      = regexp.MustCompile(`\s(?:href|src)="([^"]*)"`)
	allowedSchemes = map[string]bool{"": true, "http": true, "https": true, "mailto": true, "tel": true}
	allowedTags    = map[string]bool{"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "ul": true, "ol": true, "li": true, "blockquote": true, "pre": true, "code": true, "hr": true, "br": true, "strong": true, "em": true, "s": true, "u": true, "sub": true, "sup": true, "a": true}
	allowedAttrs   = map[string]bool{"id": true, "href": true, "class": true, "start": true, "target": true, "rel": true}
)

func TestFromMarkdown(t *testing.T) {
	n := FromMarkdown("# Title\n\nSome **bold** and [a link](https://x.test) and [bad](javascript:x).\n\n- one\n- two\n\n```go\nfmt.Println(\"<x>\")\n```\n\n> quoted\n")
	out := string(HTML(n, Options{}))
	for _, want := range []string{"<h1>Title</h1>", "<strong>bold</strong>", `<a href="https://x.test" rel="noopener">a link</a>`,
		"<li><p>one</p>", `<pre><code class="language-go">fmt.Println(&#34;&lt;x&gt;&#34;)</code></pre>`, "<blockquote>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "javascript") {
		t.Error("unsafe link kept")
	}
}
