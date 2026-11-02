package site_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"cms/internal/site"
)

func TestListColumnsMustExist(t *testing.T) {
	load := func(cols string) error {
		fsys := fstest.MapFS{
			"site.yaml": {Data: []byte("name: T\n")},
			"content-types/note.yaml": {Data: []byte("name: note\npublic: false\nlist_columns: " + cols +
				"\nfields:\n  - { name: release, type: text }\n")},
		}
		b, err := site.Load(fsys)
		if err == nil {
			err = b.Finalize()
		}
		return err
	}
	if err := load("[title, release, menu_order]"); err != nil {
		t.Fatalf("valid columns: %v", err)
	}
	if err := load("[title, relase]"); err == nil || !strings.Contains(err.Error(), `list_columns: "relase"`) {
		t.Fatalf("unknown column: got %v", err)
	}
}
