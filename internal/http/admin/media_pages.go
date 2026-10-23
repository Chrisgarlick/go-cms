package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"cms/internal/db/dbq"
	"cms/internal/media"
)

type mediaJSON struct {
	ID       uuid.UUID `json:"id"`
	Filename string    `json:"filename"`
	Mime     string    `json:"mime"`
	Size     int64     `json:"size"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Alt      string    `json:"alt"`
	Title    string    `json:"title"`
	Caption  string    `json:"caption"`
	Credit   string    `json:"credit"`
	URL      string    `json:"url"`
	Thumb    string    `json:"thumb"`
	IsImage  bool      `json:"is_image"`
	Ready    bool      `json:"ready"`
	Usage    int64     `json:"usage"`
}

func toMediaJSON(v *media.View) mediaJSON {
	m := mediaJSON{ID: v.ID, Filename: v.Filename, Mime: v.Mime, Size: v.Size, Width: v.Width, Height: v.Height,
		Alt: v.Alt, Title: v.Title, Caption: v.Caption, Credit: v.Credit, URL: v.URL, IsImage: v.IsImage, Ready: v.Ready}
	if t := v.Variant("thumb"); t != nil {
		m.Thumb = t.URL
	} else if !v.IsImage && strings.HasPrefix(v.Mime, "image/svg") {
		m.Thumb = v.URL
	}
	return m
}

func (h *Handler) mediaViews(r *http.Request, rows []dbq.ListMediaRow) []mediaJSON {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	views, _ := h.a.Media.Lookup(r.Context(), ids)
	out := make([]mediaJSON, 0, len(rows))
	for _, m := range rows {
		if v := views[m.ID]; v != nil {
			j := toMediaJSON(v)
			j.Usage = m.UsageCount
			out = append(out, j)
		}
	}
	return out
}

type mediaListData struct {
	Items  []mediaJSON
	Total  int64
	Query  string
	Kind   string
	Unused bool
	Page   int
	Pages  int
	MaxMB  int
}

func (h *Handler) mediaFilter(r *http.Request) media.ListFilter {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	return media.ListFilter{Query: strings.TrimSpace(r.URL.Query().Get("q")), Kind: r.URL.Query().Get("kind"),
		Unused: r.URL.Query().Get("unused") != "", Limit: 48, Offset: int32((page - 1) * 48)}
}

func (h *Handler) mediaList(w http.ResponseWriter, r *http.Request) {
	f := h.mediaFilter(r)
	rows, total, err := h.a.Media.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page := int(f.Offset/48) + 1
	h.render(w, r, "media", http.StatusOK, h.page(r, "Media", "media", mediaListData{Items: h.mediaViews(r, rows), Total: total,
		Query: f.Query, Kind: f.Kind, Unused: f.Unused, Page: page, Pages: int((total + 47) / 48), MaxMB: h.a.Cfg.Media.MaxUploadMB}))
}

// mediaJSONList feeds the picker in the editor.
func (h *Handler) mediaJSONList(w http.ResponseWriter, r *http.Request) {
	f := h.mediaFilter(r)
	rows, total, err := h.a.Media.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": h.mediaViews(r, rows), "total": total})
}

// mediaUpload accepts one or more files (multipart, streamed part by part).
func (h *Handler) mediaUpload(w http.ResponseWriter, r *http.Request) {
	limit := int64(h.a.Cfg.Media.MaxUploadMB)<<20 + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, limit*10)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Expected a file upload."})
		return
	}
	var items []mediaJSON
	var errs []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		m, _, err := h.a.Media.Upload(r.Context(), media.UploadInput{Filename: part.FileName(), Body: part})
		part.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				errs = append(errs, part.FileName()+": file is too large")
				break
			}
			errs = append(errs, part.FileName()+": "+err.Error())
			continue
		}
		views, _ := h.a.Media.Lookup(r.Context(), []uuid.UUID{m.ID})
		if v := views[m.ID]; v != nil {
			items = append(items, toMediaJSON(v))
		}
	}
	status := http.StatusOK
	if len(items) == 0 && len(errs) > 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"items": items, "errors": errs})
}

type mediaEditData struct {
	Item  mediaJSON
	Raw   dbq.Medium
	Usage []dbq.MediaUsageRow
}

func (h *Handler) mediaEdit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	m, err := h.a.Media.Get(r.Context(), id)
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	views, _ := h.a.Media.Lookup(r.Context(), []uuid.UUID{id})
	usage, _ := h.a.Media.Usage(r.Context(), id)
	h.render(w, r, "media-edit", http.StatusOK, h.page(r, m.Filename, "media", mediaEditData{Item: toMediaJSON(views[id]), Raw: m, Usage: usage}))
}

func (h *Handler) mediaUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	fx, _ := strconv.ParseFloat(r.PostFormValue("focal_x"), 32)
	fy, _ := strconv.ParseFloat(r.PostFormValue("focal_y"), 32)
	if r.PostFormValue("focal_x") == "" {
		fx, fy = 0.5, 0.5
	}
	_, err = h.a.Media.Update(r.Context(), id, media.MetaInput{Alt: r.PostFormValue("alt"), Title: r.PostFormValue("title"),
		Caption: r.PostFormValue("caption"), Credit: r.PostFormValue("credit"), FocalX: float32(fx), FocalY: float32(fy)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if isJSON(r) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	http.Redirect(w, r, "/admin/media/"+id.String()+"?ok=saved", http.StatusSeeOther)
}

func (h *Handler) mediaDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	if err := h.a.Media.Delete(r.Context(), id, r.PostFormValue("force") != ""); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/media/?ok=deleted", http.StatusSeeOther)
}
