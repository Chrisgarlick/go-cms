package render

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"cms/internal/blocks"
	"cms/internal/richtext"
)

// Funcs is the restricted template function set (spec 4.12): no filesystem,
// network, exec or raw-HTML escape hatch.
func Funcs() template.FuncMap {
	return template.FuncMap{
		"date":      formatDate,
		"isodate":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"year":      func(t time.Time) int { return t.Year() },
		"now":       time.Now,
		"truncate":  truncate,
		"lower":     strings.ToLower,
		"upper":     strings.ToUpper,
		"title":     titleCase,
		"trim":      strings.TrimSpace,
		"contains":  strings.Contains,
		"hasPrefix": strings.HasPrefix,
		"join":      strings.Join,
		"split":     strings.Split,
		"replace":   strings.ReplaceAll,
		"slug":      richtext.Slugify,
		"default":   defaultVal,
		"empty":     isEmpty,
		"dict":      dict,
		"list":      func(v ...any) []any { return v },
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"mul":       func(a, b int) int { return a * b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mod": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a % b
		},
		"seq": func(n int) []int {
			if n > 1000 {
				n = 1000
			}
			out := make([]int, n)
			for i := range out {
				out[i] = i + 1
			}
			return out
		},
		"plural": func(n any, one, many string) string {
			if toInt(n) == 1 {
				return one
			}
			return many
		},
		"str":   func(v any) string { return fmt.Sprint(v) },
		"int":   toInt,
		"nl2br": nl2br,
		"lines": func(s string) []string {
			return strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n")
		},
		"words":   func(s string) int { return len(strings.Fields(s)) },
		"field":   field,
		"toJSON":  func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"blockID": blocks.NewID,
		"safeURL": func(u string) string {
			if s, ok := richtext.SafeURL(u); ok {
				return s
			}
			return "#"
		},
		"external": func(u string) bool { return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") },
		"inline":   inline,
		"cond": func(ok bool, yes, no any) any {
			if ok {
				return yes
			}
			return no
		},
		"append": func(l []any, v ...any) []any { return append(append([]any{}, l...), v...) },
		"pad": func(n any, width int) string {
			return fmt.Sprintf("%0*d", width, toInt(n))
		},
	}
}

var (
	inlineStrong = regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*`)
	inlineEm     = regexp.MustCompile(`\*(\S(?:.*?\S)?)\*`)
)

// inline escapes plain text, then turns **strong** and *em* into markup, for
// headings an editor types in a text field ("Websites that *stay* fast").
func inline(v any) template.HTML {
	s := template.HTMLEscapeString(fmt.Sprint(v))
	if v == nil {
		s = ""
	}
	s = inlineStrong.ReplaceAllString(s, "<strong>$1</strong>")
	s = inlineEm.ReplaceAllString(s, "<em>$1</em>")
	return template.HTML(s)
}

func formatDate(t time.Time, layout ...string) string {
	if t.IsZero() {
		return ""
	}
	l := "2 January 2006"
	if len(layout) > 0 && layout[0] != "" {
		l = layout[0]
	}
	return t.Format(l)
}

func truncate(n int, s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	out := string(r)
	if i := strings.LastIndex(out, " "); i > n/2 {
		out = out[:i]
	}
	return strings.TrimRight(out, " ,.;:-") + "…"
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = strings.ToUpper(string(r)) + w[size:]
	}
	return strings.Join(words, " ")
}

func defaultVal(def, v any) any {
	if isEmpty(v) {
		return def
	}
	return v
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int64, reflect.Int32:
		return rv.Int() == 0
	case reflect.Float64, reflect.Float32:
		return rv.Float() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		_, _ = fmt.Sscanf(t, "%d", &n)
		return n
	}
	return 0
}

// nl2br escapes text and turns newlines into <br>.
func nl2br(s string) template.HTML {
	return template.HTML(strings.ReplaceAll(html.EscapeString(strings.TrimSpace(s)), "\n", "<br>\n"))
}

// field reads a nested map value: field .Data "cta" "url".
func field(m any, path ...string) any {
	cur := m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// faqItems extracts Q/A pairs from accordion blocks with faq_schema enabled.
func faqItems(raw json.RawMessage) [][2]string {
	var out [][2]string
	blocks.Walk(blocks.Parse(raw), func(b *blocks.Block) {
		if b.Type != "accordion" {
			return
		}
		var d struct {
			FAQ   bool `json:"faq_schema"`
			Items []struct {
				Q string          `json:"question"`
				A json.RawMessage `json:"answer"`
			} `json:"items"`
		}
		if json.Unmarshal(b.Data, &d) != nil || !d.FAQ {
			return
		}
		for _, it := range d.Items {
			out = append(out, [2]string{it.Q, richtext.PlainTextJSON(it.A)})
		}
	})
	return out
}
