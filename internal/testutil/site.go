package testutil

import (
	"testing"
	"testing/fstest"

	"cms/internal/blocks"
	"cms/internal/site"
)

// BlogBundle is a small site used by service tests.
func BlogBundle(t testing.TB) *site.Bundle {
	t.Helper()
	fsys := fstest.MapFS{
		"site.yaml": {Data: []byte("name: Test Blog\ntimezone: Europe/London\n")},
		"content-types/post.yaml": {Data: []byte(`name: post
url: /blog/{slug}
archive: /blog
taxonomies: [category]
feed: true
blocks: { allowed: [rich-text, image, quote, html, columns, heading], default: [rich-text] }
fields:
  - { name: subtitle, type: text, max_length: 120, searchable: true }
`)},
		"content-types/page.yaml": {Data: []byte(`name: page
hierarchical: true
url: /{path}
blocks: { default: [rich-text] }
`)},
		"taxonomies/category.yaml": {Data: []byte("name: category\nhierarchical: true\nurl: /category/{slug}\n")},
	}
	b, err := site.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	b.AddBuiltinBlocks(blocks.Builtin())
	if err := b.Finalize(); err != nil {
		t.Fatal(err)
	}
	return b
}
