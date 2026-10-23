package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"cms/internal/db/dbq"
)

// SchemaChange is one difference between the bundle and the database.
type SchemaChange struct {
	Kind   string `json:"kind"` // content_type | taxonomy | field_group | block
	Name   string `json:"name"`
	Action string `json:"action"` // add | update | remove
}

type schemaRow struct {
	kind, name string
	config     []byte
	hash       string
}

func (b *Bundle) rows() []schemaRow {
	var out []schemaRow
	add := func(kind, name string, v any) {
		cfg, _ := json.Marshal(v)
		h := sha256.Sum256(cfg)
		out = append(out, schemaRow{kind: kind, name: name, config: cfg, hash: hex.EncodeToString(h[:8])})
	}
	for n, t := range b.Types {
		add("content_type", n, t)
	}
	for n, t := range b.Taxonomies {
		add("taxonomy", n, t)
	}
	for n, g := range b.FieldGroups {
		add("field_group", n, g)
	}
	for n, d := range b.Blocks {
		add("block", n, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].name < out[j].name
	})
	return out
}

// Diff compares the bundle with the schema mirror in the database.
func (b *Bundle) Diff(ctx context.Context, q *dbq.Queries) ([]SchemaChange, error) {
	current := map[string]map[string]string{"content_type": {}, "taxonomy": {}, "field_group": {}, "block": {}}
	ct, err := q.ListContentTypeHashes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range ct {
		current["content_type"][r.Name] = r.SourceHash
	}
	tx, err := q.ListTaxonomyHashes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range tx {
		current["taxonomy"][r.Name] = r.SourceHash
	}
	fg, err := q.ListFieldGroupHashes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range fg {
		current["field_group"][r.Name] = r.SourceHash
	}
	bl, err := q.ListBlockDefinitionHashes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range bl {
		current["block"][r.Name] = r.SourceHash
	}
	var changes []SchemaChange
	for _, r := range b.rows() {
		h, ok := current[r.kind][r.name]
		switch {
		case !ok:
			changes = append(changes, SchemaChange{Kind: r.kind, Name: r.name, Action: "add"})
		case h != r.hash:
			changes = append(changes, SchemaChange{Kind: r.kind, Name: r.name, Action: "update"})
		}
		delete(current[r.kind], r.name)
	}
	for kind, rest := range current {
		for name := range rest {
			changes = append(changes, SchemaChange{Kind: kind, Name: name, Action: "remove"})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Kind != changes[j].Kind {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Name < changes[j].Name
	})
	return changes, nil
}

// Apply writes the bundle's schema into the mirror tables. Removing a content
// type or taxonomy that still has content is refused.
func (b *Bundle) Apply(ctx context.Context, q *dbq.Queries) ([]SchemaChange, error) {
	changes, err := b.Diff(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Action != "remove" {
			continue
		}
		switch c.Kind {
		case "content_type":
			n, err := q.CountEntriesOfType(ctx, c.Name)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, fmt.Errorf("content type %q was removed from the bundle but still has %d entries; delete them first or keep the type", c.Name, n)
			}
		case "taxonomy":
			n, err := q.CountTermsOfTaxonomy(ctx, c.Name)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, fmt.Errorf("taxonomy %q was removed from the bundle but still has %d terms", c.Name, n)
			}
		}
	}
	for _, r := range b.rows() {
		var err error
		switch r.kind {
		case "content_type":
			err = q.UpsertContentType(ctx, dbq.UpsertContentTypeParams{Name: r.name, Config: r.config, SourceHash: r.hash})
		case "taxonomy":
			err = q.UpsertTaxonomy(ctx, dbq.UpsertTaxonomyParams{Name: r.name, Config: r.config, SourceHash: r.hash})
		case "field_group":
			err = q.UpsertFieldGroup(ctx, dbq.UpsertFieldGroupParams{Name: r.name, Config: r.config, SourceHash: r.hash})
		case "block":
			err = q.UpsertBlockDefinition(ctx, dbq.UpsertBlockDefinitionParams{Name: r.name, Config: r.config, SourceHash: r.hash})
		}
		if err != nil {
			return nil, err
		}
	}
	for _, c := range changes {
		if c.Action != "remove" {
			continue
		}
		var err error
		switch c.Kind {
		case "content_type":
			err = q.DeleteContentType(ctx, c.Name)
		case "taxonomy":
			err = q.DeleteTaxonomy(ctx, c.Name)
		case "field_group":
			err = q.DeleteFieldGroup(ctx, c.Name)
		case "block":
			err = q.DeleteBlockDefinition(ctx, c.Name)
		}
		if err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// ApplySchemaForTest is Apply without the change list.
func ApplySchemaForTest(ctx context.Context, q *dbq.Queries, b *Bundle) error {
	_, err := b.Apply(ctx, q)
	return err
}
