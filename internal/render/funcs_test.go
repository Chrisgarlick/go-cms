package render

import (
	"strings"
	"testing"
	"text/template"
)

func TestFromJSON(t *testing.T) {
	tm := template.Must(template.New("t").Funcs(template.FuncMap(Funcs())).Parse(
		`{{$d := fromJSON .}}{{range $d.categories}}{{.name}}:{{range .features}}{{.name}}={{.kritano}};{{end}}{{end}}|{{fromJSON "not json"}}`))
	var b strings.Builder
	if err := tm.Execute(&b, `{"categories": [{"name": "SEO", "features": [{"name": "Audit", "kritano": true}]}]}`); err != nil {
		t.Fatal(err)
	}
	if b.String() != "SEO:Audit=true;|<no value>" { // invalid JSON is nil
		t.Errorf("got %q", b.String())
	}
}
