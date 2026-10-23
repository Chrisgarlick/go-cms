package app

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cms/internal/fields"
	"cms/internal/forms"
	"cms/internal/site"
	"cms/internal/web"
)

// formToken hands a form its signed token (fetched when a visitor starts filling it in).
func (a *App) formToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Cache-Control", "no-store")
	if a.Forms.Form(name) == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such form"})
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"token": a.Forms.Token(name)})
}

// formSubmit accepts a submission as JSON or as an ordinary form post.
func (a *App) formSubmit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Cache-Control", "no-store")
	f := a.Forms.Form(name)
	if f == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	raw := map[string]any{}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	wantJSON := ct == "application/json" || strings.Contains(r.Header.Get("Accept"), "application/json")
	if ct == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "The form could not be read."})
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		for k, v := range r.PostForm {
			if len(v) == 1 {
				raw[k] = v[0]
			} else {
				raw[k] = v
			}
		}
	}
	token, _ := raw["_token"].(string)
	honeypot, _ := raw[forms.Honeypot].(string)
	values := coerce(f, raw)
	_, err := a.Forms.Submit(r.Context(), name, values, token, honeypot, web.ClientIP(r.Context()))
	status, body := http.StatusOK, map[string]any{"ok": true, "message": f.Success}
	var fe fields.Errors
	switch {
	case err == nil, errors.Is(err, forms.ErrSpam):
		// A filled honeypot gets the same answer as a real submission.
	case errors.As(err, &fe):
		status, body = http.StatusUnprocessableEntity, map[string]any{"error": "Please check the highlighted fields.", "errors": fe}
	case errors.Is(err, forms.ErrToken), errors.Is(err, forms.ErrTooFast):
		status, body = http.StatusBadRequest, map[string]any{"error": err.Error()}
	case errors.Is(err, forms.ErrRateLimited):
		status, body = http.StatusTooManyRequests, map[string]any{"error": err.Error()}
	default:
		a.Log.Error("form submission failed", "form", name, "err", err, "request_id", web.RequestID(r.Context()))
		status, body = http.StatusInternalServerError, map[string]any{"error": "Sorry, something went wrong. Please try again, or email instead."}
	}
	if wantJSON {
		writeJSONStatus(w, status, body)
		return
	}
	// Without JavaScript: back to the page the form was on.
	back := "/"
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Path != "" && (ref.Host == "" || ref.Host == r.Host) {
		back = ref.Path
	}
	result := "sent"
	if status != http.StatusOK {
		result = "error"
	}
	http.Redirect(w, r, back+"?form="+url.QueryEscape(name)+"&result="+result+"#"+name, http.StatusSeeOther)
}

// coerce turns posted strings into the types the field definitions expect.
func coerce(f *site.Form, raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, fd := range f.Fields {
		v, ok := raw[fd.Name]
		if !ok {
			continue
		}
		switch fd.Type {
		case fields.Number:
			if s, ok := v.(string); ok {
				if s = strings.TrimSpace(s); s == "" {
					v = nil
				} else if n, err := strconv.ParseFloat(s, 64); err == nil {
					v = n
				}
			}
		case fields.Boolean:
			if s, ok := v.(string); ok {
				v = s == "1" || s == "true" || s == "on" || s == "yes"
			}
		case fields.MultiSelect, fields.Checkbox:
			switch t := v.(type) {
			case string:
				if t == "" {
					v = []any{}
				} else {
					v = []any{t}
				}
			case []string:
				l := make([]any, len(t))
				for i, x := range t {
					l[i] = x
				}
				v = l
			}
		default:
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
		}
		out[fd.Name] = v
	}
	return out
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
