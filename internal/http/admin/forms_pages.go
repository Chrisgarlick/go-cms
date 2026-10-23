package admin

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"cms/internal/fields"
	"cms/internal/forms"
	"cms/internal/site"
)

type formRow struct {
	Form          *site.Form
	Total, Unread int64
}

func (h *Handler) formsPage(w http.ResponseWriter, r *http.Request) {
	counts, err := h.a.Forms.Counts(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var rows []formRow
	for _, f := range h.a.Site().Forms {
		c := counts[f.Name]
		rows = append(rows, formRow{Form: f, Total: c.Total, Unread: c.Unread})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Form.Label < rows[j].Form.Label })
	h.render(w, r, "forms", http.StatusOK, h.page(r, "Form submissions", "forms", rows))
}

type submissionsData struct {
	Form        *site.Form
	Submissions []*forms.Submission
	Page        int
	More        bool
}

const submissionsPerPage = 50

func (h *Handler) formSubmissions(w http.ResponseWriter, r *http.Request) {
	f := h.a.Forms.Form(r.PathValue("form"))
	if f == nil {
		h.error(w, r, http.StatusNotFound, "There is no such form.")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	subs, err := h.a.Forms.List(r.Context(), f.Name, submissionsPerPage+1, (page-1)*submissionsPerPage)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := submissionsData{Form: f, Page: page}
	if len(subs) > submissionsPerPage {
		d.More, subs = true, subs[:submissionsPerPage]
	}
	d.Submissions = subs
	h.render(w, r, "form-submissions", http.StatusOK, h.page(r, f.Label, "forms", d))
}

type submissionField struct {
	Label, Value string
	Long         bool
}

func (h *Handler) formSubmission(w http.ResponseWriter, r *http.Request) {
	f := h.a.Forms.Form(r.PathValue("form"))
	id, err := uuid.Parse(r.PathValue("id"))
	if f == nil || err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	sub, err := h.a.Forms.Get(r.Context(), id)
	if err != nil || sub.Form != f.Name {
		h.error(w, r, http.StatusNotFound, "This submission doesn't exist. It may have been deleted.")
		return
	}
	_ = h.a.Forms.MarkRead(r.Context(), id)
	var out []submissionField
	for _, fd := range f.Fields {
		label := fd.Label
		if label == "" {
			label = fields.Humanize(fd.Name)
		}
		v := forms.Format(sub.Values[fd.Name])
		out = append(out, submissionField{Label: label, Value: v, Long: fd.Type == fields.Textarea})
	}
	h.render(w, r, "form-submission", http.StatusOK, h.page(r, f.Label, "forms", map[string]any{"Form": f, "Sub": sub, "Fields": out}))
}

func (h *Handler) formSubmissionDelete(w http.ResponseWriter, r *http.Request) {
	form := r.PathValue("form")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	if err := h.a.Forms.Delete(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/forms/"+form+"/?ok=deleted", http.StatusSeeOther)
}
