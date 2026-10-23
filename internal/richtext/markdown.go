package richtext

import (
	"regexp"
	"strings"
)

// FromMarkdown converts a practical subset of Markdown into a document:
// paragraphs, #-headings, bullet and numbered lists, blockquotes, fenced code,
// horizontal rules, **bold**, *italic*, `code`, ~~strike~~ and [links](url).
// Output passes through the same sanitiser as editor input.
func FromMarkdown(src string) *Node {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	doc := &Node{Type: "doc"}
	var para []string
	flush := func() {
		if len(para) > 0 {
			doc.Content = append(doc.Content, Node{Type: "paragraph", Content: inline(strings.Join(para, " "))})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, "```"):
			flush()
			lang := strings.TrimSpace(strings.TrimPrefix(t, "```"))
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			n := Node{Type: "codeBlock", Content: []Node{{Type: "text", Text: strings.Join(code, "\n")}}}
			if lang != "" && validLang(lang) {
				n.Attrs = map[string]any{"language": lang}
			}
			doc.Content = append(doc.Content, n)
		case strings.HasPrefix(t, "#"):
			flush()
			lvl := 0
			for lvl < len(t) && t[lvl] == '#' {
				lvl++
			}
			if lvl > 6 {
				lvl = 6
			}
			doc.Content = append(doc.Content, Node{Type: "heading", Attrs: map[string]any{"level": lvl},
				Content: inline(strings.TrimSpace(t[lvl:]))})
		case t == "---" || t == "***":
			flush()
			doc.Content = append(doc.Content, Node{Type: "horizontalRule"})
		case strings.HasPrefix(t, "> "):
			flush()
			var q []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				q = append(q, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
			}
			i--
			doc.Content = append(doc.Content, Node{Type: "blockquote", Content: []Node{{Type: "paragraph", Content: inline(strings.Join(q, " "))}}})
		case bulletRe.MatchString(t), orderedRe.MatchString(t):
			flush()
			ordered := orderedRe.MatchString(t)
			list := Node{Type: "bulletList"}
			if ordered {
				list.Type = "orderedList"
			}
			for ; i < len(lines); i++ {
				lt := strings.TrimSpace(lines[i])
				var item string
				if !ordered && bulletRe.MatchString(lt) {
					item = bulletRe.ReplaceAllString(lt, "")
				} else if ordered && orderedRe.MatchString(lt) {
					item = orderedRe.ReplaceAllString(lt, "")
				} else {
					break
				}
				list.Content = append(list.Content, Node{Type: "listItem", Content: []Node{{Type: "paragraph", Content: inline(item)}}})
			}
			i--
			doc.Content = append(doc.Content, list)
		default:
			para = append(para, t)
		}
	}
	flush()
	count := 0
	clean, err := sanitize(*doc, 0, &count, "doc")
	if err != nil {
		return &Node{Type: "doc"}
	}
	return &clean
}

var (
	bulletRe  = regexp.MustCompile(`^[-*+]\s+`)
	orderedRe = regexp.MustCompile(`^\d+[.)]\s+`)
	inlineRe  = regexp.MustCompile("\\*\\*([^*]+)\\*\\*|\\*([^*]+)\\*|`([^`]+)`|~~([^~]+)~~|\\[([^\\]]+)\\]\\(([^)\\s]+)\\)")
)

func inline(s string) []Node {
	var out []Node
	last := 0
	for _, m := range inlineRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			out = append(out, Node{Type: "text", Text: s[last:m[0]]})
		}
		switch {
		case m[2] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[2]:m[3]], Marks: []Mark{{Type: "bold"}}})
		case m[4] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[4]:m[5]], Marks: []Mark{{Type: "italic"}}})
		case m[6] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[6]:m[7]], Marks: []Mark{{Type: "code"}}})
		case m[8] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[8]:m[9]], Marks: []Mark{{Type: "strike"}}})
		case m[10] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[10]:m[11]], Marks: []Mark{{Type: "link", Attrs: map[string]any{"href": s[m[12]:m[13]]}}}})
		}
		last = m[1]
	}
	if last < len(s) {
		out = append(out, Node{Type: "text", Text: s[last:]})
	}
	return out
}
