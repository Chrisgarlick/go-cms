package api

import (
	"net/http"

	"cms/internal/version"
)

// openapi publishes a hand-maintained OpenAPI 3.1 description of v1. CI diffs
// it against the previous release to catch breaking changes (spec 9.2).
func (h *Handler) openapi(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"type": "object", "properties": map[string]any{
		"id": str("uuid"), "type": str(""), "title": str(""), "slug": str(""), "url": str(""), "excerpt": str(""),
		"published_at": str("date-time"), "updated_at": str("date-time"),
		"author":         map[string]any{"type": "object", "properties": map[string]any{"name": str(""), "bio": str("")}},
		"featured_image": map[string]any{"$ref": "#/components/schemas/Media"},
		"terms":          map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Term"}}},
		"fields":         map[string]any{"type": "object"}, "blocks": map[string]any{"type": "array"}, "html": str(""),
	}}
	list := func(item string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{"data": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/" + item}},
			"total": map[string]any{"type": "integer"}, "next_cursor": map[string]any{"type": []string{"string", "null"}}}}
	}
	param := func(name, in, desc string, required bool) map[string]any {
		return map[string]any{"name": name, "in": in, "description": desc, "required": required, "schema": map[string]any{"type": "string"}}
	}
	ok := func(schema map[string]any) map[string]any {
		return map[string]any{"200": map[string]any{"description": "OK", "content": map[string]any{"application/json": map[string]any{"schema": schema}}}}
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": h.a.Site().Config.Name + " API", "version": "v1 (cms " + version.Version + ")", "description": "Read-only access to published content. Write endpoints arrive in v1.x."},
		"servers": []any{map[string]any{"url": h.a.Cfg.BaseURL + "/api/v1"}},
		"components": map[string]any{
			"securitySchemes": map[string]any{"token": map[string]any{"type": "http", "scheme": "bearer", "description": "API token (cms_pat_...). Optional for public content."}},
			"schemas": map[string]any{
				"Entry": entry,
				"Media": map[string]any{"type": "object", "properties": map[string]any{"id": str("uuid"), "url": str(""), "alt": str(""), "width": num(), "height": num(), "variants": map[string]any{"type": "object"}}},
				"Term":  map[string]any{"type": "object", "properties": map[string]any{"id": str("uuid"), "name": str(""), "slug": str("")}},
			},
		},
		"paths": map[string]any{
			"/types": map[string]any{"get": map[string]any{"summary": "List public content types", "responses": ok(map[string]any{"type": "object"})}},
			"/content/{type}": map[string]any{"get": map[string]any{"summary": "List published entries",
				"parameters": []any{param("type", "path", "Content type", true), param("limit", "query", "1-100 (default 20)", false),
					param("cursor", "query", "next_cursor from the previous page", false), param("sort", "query", "newest | oldest | title | menu_order", false),
					param("taxonomy", "query", "Filter by taxonomy...", false), param("term", "query", "...and term slug", false),
					param("fields", "query", "Comma-separated top-level fields to return", false)},
				"responses": ok(list("Entry"))}},
			"/content/{type}/{ref}": map[string]any{"get": map[string]any{"summary": "Get one published entry by slug or id",
				"parameters": []any{param("type", "path", "Content type", true), param("ref", "path", "Slug or UUID", true),
					param("render", "query", "html: include rendered block HTML", false), param("fields", "query", "Comma-separated fields", false)},
				"responses": ok(map[string]any{"type": "object", "properties": map[string]any{"data": map[string]any{"$ref": "#/components/schemas/Entry"}}})}},
			"/taxonomies/{tax}/terms": map[string]any{"get": map[string]any{"summary": "List terms with published counts",
				"parameters": []any{param("tax", "path", "Taxonomy", true)}, "responses": ok(list("Term"))}},
			"/menus/{name}": map[string]any{"get": map[string]any{"summary": "Resolved menu", "parameters": []any{param("name", "path", "Menu location", true)}, "responses": ok(map[string]any{"type": "object"})}},
			"/search":       map[string]any{"get": map[string]any{"summary": "Full-text search", "parameters": []any{param("q", "query", "Query", true)}, "responses": ok(map[string]any{"type": "object"})}},
			"/site":         map[string]any{"get": map[string]any{"summary": "Site information", "responses": ok(map[string]any{"type": "object"})}},
		},
	}
	writeJSON(w, doc)
}

func str(format string) map[string]any {
	m := map[string]any{"type": "string"}
	if format != "" {
		m["format"] = format
	}
	return m
}

func num() map[string]any { return map[string]any{"type": "integer"} }
