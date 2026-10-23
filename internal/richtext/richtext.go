// Package richtext validates ProseMirror/Tiptap JSON against a strict node and
// mark allow-list and renders it to HTML with its own escaping (spec 7.2).
// Stored HTML is never passed through: anything outside the schema is dropped.
package richtext

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"strconv"
	"strings"
)

// Node is one ProseMirror node.
type Node struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []Node         `json:"content,omitempty"`
	Marks   []Mark         `json:"marks,omitempty"`
	Text    string         `json:"text,omitempty"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

const (
	maxDepth = 24
	maxNodes = 20000
)

var ErrInvalid = errors.New("richtext: invalid document")

// blockNodes may appear at doc level or inside block containers.
var blockNodes = map[string]bool{
	"paragraph": true, "heading": true, "bulletList": true, "orderedList": true,
	"blockquote": true, "codeBlock": true, "horizontalRule": true,
}

var inlineNodes = map[string]bool{"text": true, "hardBreak": true}

var allowedMarks = map[string]bool{
	"bold": true, "italic": true, "strike": true, "code": true, "link": true,
	"underline": true, "subscript": true, "superscript": true,
}

// Parse decodes and sanitises a document. Unknown nodes and marks are dropped
// (not errors) so content survives editor upgrades; structural limits are errors.
func Parse(raw []byte) (*Node, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return &Node{Type: "doc"}, nil
	}
	var n Node
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if n.Type != "doc" {
		return nil, fmt.Errorf("%w: root must be doc", ErrInvalid)
	}
	count := 0
	clean, err := sanitize(n, 0, &count, "doc")
	if err != nil {
		return nil, err
	}
	return &clean, nil
}

// Normalize returns the sanitised document as JSON for storage.
func Normalize(raw []byte) (json.RawMessage, error) {
	n, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(n)
}

func sanitize(n Node, depth int, count *int, parent string) (Node, error) {
	*count++
	if *count > maxNodes {
		return Node{}, fmt.Errorf("%w: more than %d nodes", ErrInvalid, maxNodes)
	}
	if depth > maxDepth {
		return Node{}, fmt.Errorf("%w: nested deeper than %d", ErrInvalid, maxDepth)
	}
	out := Node{Type: n.Type}
	switch n.Type {
	case "text":
		out.Text = n.Text
		if parent == "codeBlock" {
			return out, nil // code blocks hold plain text only
		}
		for _, m := range n.Marks {
			if cm, ok := cleanMark(m); ok {
				out.Marks = append(out.Marks, cm)
			}
		}
		return out, nil
	case "heading":
		lvl := intAttr(n.Attrs, "level", 2)
		if lvl < 1 || lvl > 6 {
			lvl = 2
		}
		out.Attrs = map[string]any{"level": lvl}
	case "orderedList":
		if s := intAttr(n.Attrs, "start", 1); s != 1 {
			out.Attrs = map[string]any{"start": s}
		}
	case "codeBlock":
		if lang := strAttr(n.Attrs, "language"); lang != "" && validLang(lang) {
			out.Attrs = map[string]any{"language": lang}
		}
	case "paragraph", "doc", "bulletList", "listItem", "blockquote", "horizontalRule", "hardBreak":
	default:
		return Node{}, errDrop
	}
	allowedChild := childPolicy(n.Type)
	for _, c := range n.Content {
		if !allowedChild(c.Type) {
			// Lift text out of unknown inline wrappers; drop everything else.
			continue
		}
		cc, err := sanitize(c, depth+1, count, n.Type)
		if errors.Is(err, errDrop) {
			continue
		}
		if err != nil {
			return Node{}, err
		}
		out.Content = append(out.Content, cc)
	}
	return out, nil
}

var errDrop = errors.New("drop")

func childPolicy(t string) func(string) bool {
	switch t {
	case "doc", "blockquote", "listItem":
		return func(c string) bool { return blockNodes[c] }
	case "bulletList", "orderedList":
		return func(c string) bool { return c == "listItem" }
	case "paragraph", "heading":
		return func(c string) bool { return inlineNodes[c] }
	case "codeBlock":
		return func(c string) bool { return c == "text" }
	}
	return func(string) bool { return false }
}

func cleanMark(m Mark) (Mark, bool) {
	if !allowedMarks[m.Type] {
		return Mark{}, false
	}
	if m.Type != "link" {
		return Mark{Type: m.Type}, true
	}
	href, ok := SafeURL(strAttr(m.Attrs, "href"))
	if !ok {
		return Mark{}, false
	}
	attrs := map[string]any{"href": href}
	if strAttr(m.Attrs, "target") == "_blank" {
		attrs["target"] = "_blank"
	}
	return Mark{Type: "link", Attrs: attrs}, true
}

// SafeURL allows http(s), mailto, tel, and relative URLs. Everything else
// (javascript:, data:, vbscript:, protocol-relative to other schemes) is refused.
func SafeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto", "tel":
		return u.String(), true
	case "":
		if strings.HasPrefix(raw, "//") {
			return "", false
		}
		return u.String(), true
	}
	return "", false
}

func validLang(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '+' || r == '#') {
			return false
		}
	}
	return true
}

func intAttr(a map[string]any, k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func strAttr(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// Options adjust rendering.
type Options struct {
	// HeadingOffset shifts heading levels (e.g. 1 turns h2 into h3), capped at h6.
	HeadingOffset int
	// AnchorHeadings adds id attributes derived from heading text.
	AnchorHeadings bool
}

// HTML renders a sanitised document. Output is safe to embed: every text and
// attribute value is escaped here.
func HTML(n *Node, o Options) template.HTML {
	if n == nil {
		return ""
	}
	var b strings.Builder
	ids := map[string]int{}
	renderChildren(&b, n.Content, o, ids)
	return template.HTML(b.String())
}

// RenderJSON parses and renders in one step; invalid input renders nothing.
func RenderJSON(raw []byte, o Options) template.HTML {
	n, err := Parse(raw)
	if err != nil {
		return ""
	}
	return HTML(n, o)
}

func renderChildren(b *strings.Builder, ns []Node, o Options, ids map[string]int) {
	for i := range ns {
		render(b, &ns[i], o, ids)
	}
}

func render(b *strings.Builder, n *Node, o Options, ids map[string]int) {
	switch n.Type {
	case "paragraph":
		b.WriteString("<p>")
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</p>\n")
	case "heading":
		lvl := intAttr(n.Attrs, "level", 2) + o.HeadingOffset
		if lvl > 6 {
			lvl = 6
		}
		if lvl < 1 {
			lvl = 1
		}
		tag := "h" + strconv.Itoa(lvl)
		b.WriteString("<" + tag)
		if o.AnchorHeadings {
			id := Slugify(PlainText(n))
			if id != "" {
				if c := ids[id]; c > 0 {
					ids[id] = c + 1
					id += "-" + strconv.Itoa(c+1)
				} else {
					ids[id] = 1
				}
				b.WriteString(` id="` + html.EscapeString(id) + `"`)
			}
		}
		b.WriteString(">")
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</" + tag + ">\n")
	case "bulletList":
		b.WriteString("<ul>\n")
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</ul>\n")
	case "orderedList":
		if s := intAttr(n.Attrs, "start", 1); s != 1 {
			b.WriteString(`<ol start="` + strconv.Itoa(s) + `">` + "\n")
		} else {
			b.WriteString("<ol>\n")
		}
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</ol>\n")
	case "listItem":
		b.WriteString("<li>")
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</li>\n")
	case "blockquote":
		b.WriteString("<blockquote>\n")
		renderChildren(b, n.Content, o, ids)
		b.WriteString("</blockquote>\n")
	case "codeBlock":
		b.WriteString("<pre><code")
		if lang := strAttr(n.Attrs, "language"); lang != "" {
			b.WriteString(` class="language-` + html.EscapeString(lang) + `"`)
		}
		b.WriteString(">")
		for _, c := range n.Content {
			b.WriteString(html.EscapeString(c.Text))
		}
		b.WriteString("</code></pre>\n")
	case "horizontalRule":
		b.WriteString("<hr>\n")
	case "hardBreak":
		b.WriteString("<br>")
	case "text":
		renderText(b, n)
	}
}

func renderText(b *strings.Builder, n *Node) {
	var closers []string
	for _, m := range n.Marks {
		switch m.Type {
		case "bold":
			b.WriteString("<strong>")
			closers = append(closers, "</strong>")
		case "italic":
			b.WriteString("<em>")
			closers = append(closers, "</em>")
		case "strike":
			b.WriteString("<s>")
			closers = append(closers, "</s>")
		case "underline":
			b.WriteString("<u>")
			closers = append(closers, "</u>")
		case "code":
			b.WriteString("<code>")
			closers = append(closers, "</code>")
		case "subscript":
			b.WriteString("<sub>")
			closers = append(closers, "</sub>")
		case "superscript":
			b.WriteString("<sup>")
			closers = append(closers, "</sup>")
		case "link":
			href, ok := SafeURL(strAttr(m.Attrs, "href"))
			if !ok {
				continue
			}
			b.WriteString(`<a href="` + html.EscapeString(href) + `"`)
			if strAttr(m.Attrs, "target") == "_blank" {
				b.WriteString(` target="_blank" rel="noopener noreferrer"`)
			} else if u, err := url.Parse(href); err == nil && u.Host != "" {
				b.WriteString(` rel="noopener"`)
			}
			b.WriteString(">")
			closers = append(closers, "</a>")
		}
	}
	b.WriteString(html.EscapeString(n.Text))
	for i := len(closers) - 1; i >= 0; i-- {
		b.WriteString(closers[i])
	}
}

// PlainText extracts text for search indexing and excerpts.
func PlainText(n *Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*Node)
	walk = func(n *Node) {
		if n.Type == "text" {
			b.WriteString(n.Text)
			return
		}
		if n.Type == "hardBreak" {
			b.WriteByte(' ')
			return
		}
		for i := range n.Content {
			walk(&n.Content[i])
		}
		if blockNodes[n.Type] || n.Type == "listItem" {
			b.WriteByte(' ')
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// PlainTextJSON extracts text from a stored document.
func PlainTextJSON(raw []byte) string {
	n, err := Parse(raw)
	if err != nil {
		return ""
	}
	return PlainText(n)
}

// Headings returns (level, text, anchor) for a table of contents.
type Heading struct {
	Level  int
	Text   string
	Anchor string
}

func Headings(n *Node) []Heading {
	var out []Heading
	ids := map[string]int{}
	for i := range n.Content {
		c := &n.Content[i]
		if c.Type != "heading" {
			continue
		}
		t := PlainText(c)
		id := Slugify(t)
		if cnt := ids[id]; cnt > 0 {
			ids[id] = cnt + 1
			id += "-" + strconv.Itoa(cnt+1)
		} else {
			ids[id] = 1
		}
		out = append(out, Heading{Level: intAttr(c.Attrs, "level", 2), Text: t, Anchor: id})
	}
	return out
}

// Slugify makes a lowercase ASCII slug: letters, digits and single hyphens.
func Slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(foldAccents(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 96 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

var accentFold = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "æ", "ae",
	"ç", "c", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i", "ñ", "n",
	"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o", "œ", "oe",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y", "ß", "ss",
	"&", " and ", "'", "", "’", "",
)

func foldAccents(s string) string { return accentFold.Replace(strings.ToLower(s)) }
