// Package media stores uploads and generates image variants (spec 4.4).
// Uploads stream to disk while being hashed, are identified by content (not
// extension), deduplicated by SHA-256, and never decoded inside the web or
// worker process: variants are produced by a short-lived subprocess.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "golang.org/x/image/webp"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/site"
)

var (
	ErrTooLarge  = errors.New("file is too large")
	ErrType      = errors.New("this file type is not allowed")
	ErrTooManyPx = errors.New("image dimensions are too large")
	ErrNotFound  = errors.New("media not found")
	ErrInUse     = errors.New("this file is used by published content")
)

type kind struct {
	mime  string
	exts  []string
	image bool
}

// allowed maps sniffed content types to permitted extensions.
var allowed = []kind{
	{"image/jpeg", []string{"jpg", "jpeg"}, true},
	{"image/png", []string{"png"}, true},
	{"image/gif", []string{"gif"}, true},
	{"image/webp", []string{"webp"}, true},
	{"image/avif", []string{"avif"}, true},
	{"image/svg+xml", []string{"svg"}, false},
	{"application/pdf", []string{"pdf"}, false},
	{"video/mp4", []string{"mp4", "m4v"}, false},
	{"video/webm", []string{"webm"}, false},
	{"audio/mpeg", []string{"mp3"}, false},
	{"audio/ogg", []string{"ogg", "oga"}, false},
	{"audio/wave", []string{"wav"}, false},
	{"text/plain", []string{"txt", "csv", "md"}, false},
	{"application/zip", []string{"zip", "docx", "xlsx", "pptx"}, false},
}

// Sniff identifies a file from its first bytes and checks the extension agrees.
func Sniff(head []byte, filename string) (mime string, ext string, isImage bool, err error) {
	ext = strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	mime = detect(head)
	for _, k := range allowed {
		if k.mime != mime {
			continue
		}
		for _, e := range k.exts {
			if e == ext {
				return mime, ext, k.image, nil
			}
		}
		return "", "", false, fmt.Errorf("%w: the file contents are %s but the name ends in .%s", ErrType, mime, ext)
	}
	return "", "", false, fmt.Errorf("%w (%s)", ErrType, mime)
}

func detect(head []byte) string {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch string(head[8:12]) {
		case "avif", "avis":
			return "image/avif"
		}
	}
	trimmed := bytes.TrimLeft(head, "\xef\xbb\xbf \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("<svg")) || (bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(head, []byte("<svg"))) {
		return "image/svg+xml"
	}
	ct := http.DetectContentType(head)
	ct, _, _ = strings.Cut(ct, ";")
	switch ct {
	case "audio/wav", "audio/x-wav":
		return "audio/wave"
	case "application/ogg":
		return "audio/ogg"
	}
	return strings.TrimSpace(ct)
}

type Service struct {
	DB        *db.DB
	Store     *Local
	Site      func() *site.Bundle
	Jobs      jobs.Enqueuer
	Log       *slog.Logger
	TmpDir    string
	BaseURL   string // public URL prefix for media, e.g. https://media.example.com or https://example.com/media
	MaxBytes  int64
	MaxPixels int
	Processor Processor
}

// UploadInput is one file from the admin, API or CLI.
type UploadInput struct {
	Filename string
	Body     io.Reader
	Alt      string
	Title    string
}

// Upload streams a file to storage. Returns the existing item for duplicates.
func (s *Service) Upload(ctx context.Context, in UploadInput) (dbq.Medium, bool, error) {
	a, err := actor.Require(ctx, rbac.MediaManage)
	if err != nil {
		return dbq.Medium{}, false, err
	}
	if err := os.MkdirAll(s.TmpDir, 0o700); err != nil {
		return dbq.Medium{}, false, err
	}
	tmp, err := os.CreateTemp(s.TmpDir, "upload-*")
	if err != nil {
		return dbq.Medium{}, false, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	h := sha256.New()
	head := make([]byte, 512)
	n, _ := io.ReadFull(in.Body, head)
	head = head[:n]
	// Stream the rest, hashing as we go; never hold the file in memory.
	limit := s.MaxBytes
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(io.MultiReader(bytes.NewReader(head), in.Body), limit+1))
	if err != nil {
		return dbq.Medium{}, false, err
	}
	if written > limit {
		return dbq.Medium{}, false, fmt.Errorf("%w (maximum %d MB)", ErrTooLarge, limit>>20)
	}
	if written == 0 {
		return dbq.Medium{}, false, errors.New("the file is empty")
	}
	mime, ext, isImage, err := Sniff(head, in.Filename)
	if err != nil {
		return dbq.Medium{}, false, err
	}
	if mime == "image/svg+xml" && !a.Can(rbac.RawHTML) {
		return dbq.Medium{}, false, fmt.Errorf("%w: only owners can upload SVG files", ErrType)
	}
	sum := h.Sum(nil)
	if dup, err := s.DB.Q.GetMediaBySHA(ctx, sum); err == nil {
		return dup, true, nil
	}
	var w, hgt *int32
	if isImage && mime != "image/avif" {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return dbq.Medium{}, false, err
		}
		cfg, _, err := image.DecodeConfig(tmp) // header only
		if err != nil {
			return dbq.Medium{}, false, fmt.Errorf("%w: the image could not be read", ErrType)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > s.MaxPixels {
			return dbq.Medium{}, false, fmt.Errorf("%w (%dx%d)", ErrTooManyPx, cfg.Width, cfg.Height)
		}
		cw, ch := int32(cfg.Width), int32(cfg.Height)
		w, hgt = &cw, &ch
	}
	id := uuid.Must(uuid.NewV7())
	name := SafeFilename(in.Filename, ext)
	var key string
	if isImage {
		key = originalKey(id.String(), ext)
	} else {
		key = fileKey(id.String(), name)
	}
	tmp.Close()
	if err := s.Store.PutFile(key, tmp.Name()); err != nil {
		return dbq.Medium{}, false, err
	}
	var uploader *uuid.UUID
	if a.IsUser() {
		u := a.UserID
		uploader = &u
	}
	var m dbq.Medium
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		m, err = q.CreateMedia(ctx, dbq.CreateMediaParams{ID: id, Sha256: sum, Filename: name, Mime: mime, Size: written,
			Width: w, Height: hgt, StorageKey: key, Alt: strings.TrimSpace(in.Alt), Title: strings.TrimSpace(in.Title), UploaderID: uploader})
		if err != nil {
			return err
		}
		if isImage {
			if err := s.Jobs.EnqueueTx(ctx, tx, jobs.MediaProcess{MediaID: id.String()}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Event{Action: "media.upload", ResourceType: "media", ResourceID: id.String(),
			Diff: map[string]any{"mime": mime, "size": written}})
	})
	if err != nil {
		_ = s.Store.Delete(key)
	}
	return m, false, err
}

// SafeFilename keeps a readable, ASCII-only name with the verified extension.
func SafeFilename(name, ext string) string {
	base := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(name, "\\", "/")), path.Ext(name))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(unicode.ToLower(r))
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 80 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "file"
	}
	return s + "." + ext
}

// Get returns one item.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (dbq.Medium, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return dbq.Medium{}, err
	}
	m, err := s.DB.Q.GetMedia(ctx, id)
	if db.IsNotFound(err) {
		return m, ErrNotFound
	}
	return m, err
}

type ListFilter struct {
	Query  string
	Kind   string // image | video | audio | application
	Unused bool
	Limit  int32
	Offset int32
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]dbq.ListMediaRow, int64, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 60
	}
	var q, k *string
	if f.Query != "" {
		q = &f.Query
	}
	if f.Kind != "" {
		k = &f.Kind
	}
	rows, err := s.DB.Q.ListMedia(ctx, dbq.ListMediaParams{Q: q, Kind: k, Unused: f.Unused, Lim: f.Limit, Off: f.Offset})
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if len(rows) > 0 {
		total = rows[0].Total
	}
	return rows, total, nil
}

type MetaInput struct {
	Alt, Title, Caption, Credit string
	FocalX, FocalY              float32
	Fields                      json.RawMessage
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in MetaInput) (dbq.Medium, error) {
	a, err := actor.Require(ctx, rbac.MediaManage)
	if err != nil {
		return dbq.Medium{}, err
	}
	clamp := func(v float32) float32 {
		if v < 0 || v > 1 || v != v {
			return 0.5
		}
		return v
	}
	fj, _, err := fields.Validate(s.Site().MediaFields(), in.Fields, fields.Context{CanUse: func(c string) bool { return a.Can(rbac.Cap(c)) }})
	if err != nil {
		return dbq.Medium{}, err
	}
	for _, v := range []string{in.Alt, in.Title, in.Caption, in.Credit} {
		if len([]rune(v)) > 1000 {
			return dbq.Medium{}, fields.Errors{"alt": "text fields must be at most 1000 characters"}
		}
	}
	var out dbq.Medium
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		old, err := q.GetMedia(ctx, id)
		if err != nil {
			return ErrNotFound
		}
		out, err = q.UpdateMediaMeta(ctx, dbq.UpdateMediaMetaParams{ID: id, Alt: strings.TrimSpace(in.Alt), Title: strings.TrimSpace(in.Title),
			Caption: strings.TrimSpace(in.Caption), Credit: strings.TrimSpace(in.Credit), FocalX: clamp(in.FocalX), FocalY: clamp(in.FocalY), Fields: fj})
		if err != nil {
			return err
		}
		// Alt text and captions appear in rendered pages.
		users, err := q.EntriesUsingMedia(ctx, id)
		if err != nil {
			return err
		}
		var deps []jobs.Dep
		for _, e := range users {
			deps = append(deps, jobs.Dep{Type: "entry", ID: e.String()})
		}
		deps = append(deps, jobs.Dep{Type: "media", ID: id.String()})
		if old.FocalX != out.FocalX || old.FocalY != out.FocalY {
			if err := s.Jobs.EnqueueTx(ctx, tx, jobs.MediaProcess{MediaID: id.String(), Force: true}); err != nil {
				return err
			}
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: deps}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "media.update", ResourceType: "media", ResourceID: id.String()})
	})
	return out, err
}

// Delete soft-deletes an item. Items used by content need force.
func (s *Service) Delete(ctx context.Context, id uuid.UUID, force bool) error {
	if _, err := actor.Require(ctx, rbac.MediaManage); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		users, err := q.EntriesUsingMedia(ctx, id)
		if err != nil {
			return err
		}
		if len(users) > 0 && !force {
			return fmt.Errorf("%w (%d entries)", ErrInUse, len(users))
		}
		if err := q.SoftDeleteMedia(ctx, id); err != nil {
			return err
		}
		var deps []jobs.Dep
		for _, e := range users {
			deps = append(deps, jobs.Dep{Type: "entry", ID: e.String()})
		}
		if len(deps) > 0 {
			if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: deps}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Event{Action: "media.delete", ResourceType: "media", ResourceID: id.String()})
	})
}

func (s *Service) Usage(ctx context.Context, id uuid.UUID) ([]dbq.MediaUsageRow, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	return s.DB.Q.MediaUsage(ctx, id)
}

// PurgeDeleted removes files of items soft-deleted more than `age` ago.
func (s *Service) PurgeDeleted(ctx context.Context, age time.Duration) (int, error) {
	rows, err := s.DB.Q.PurgeDeletedMedia(ctx, fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		_ = s.Store.Delete(r.StorageKey)
		_ = os.RemoveAll(filepath.Join(s.Store.Root, "public", "v", r.ID.String()))
	}
	return len(rows), nil
}

// ---------------------------------------------------------------------------
// Presets and views for rendering
// ---------------------------------------------------------------------------

// Preset is a named output size.
type Preset struct {
	Name   string
	Width  int
	Height int
	Fit    string // inside | cover
}

// DefaultPresets are always generated; themes add their own (theme.yaml).
var DefaultPresets = []Preset{
	{"thumb", 320, 320, "cover"},
	{"small", 480, 0, "inside"},
	{"medium", 800, 0, "inside"},
	{"large", 1280, 0, "inside"},
	{"xl", 1920, 0, "inside"},
}

// Presets returns the default presets plus theme image sizes.
func (s *Service) Presets() []Preset {
	out := append([]Preset{}, DefaultPresets...)
	if s.Site == nil {
		return out
	}
	names := make([]string, 0, len(s.Site().Theme.ImageSizes))
	for n := range s.Site().Theme.ImageSizes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sz := s.Site().Theme.ImageSizes[n]
		fit := sz.Fit
		if fit == "" {
			fit = "inside"
			if sz.Width > 0 && sz.Height > 0 {
				fit = "cover"
			}
		}
		replaced := false
		for i := range out {
			if out[i].Name == n {
				out[i] = Preset{n, sz.Width, sz.Height, fit}
				replaced = true
			}
		}
		if !replaced {
			out = append(out, Preset{n, sz.Width, sz.Height, fit})
		}
	}
	return out
}

// View is what templates and the admin see for one item.
type View struct {
	ID       uuid.UUID
	Filename string
	Mime     string
	Size     int64
	Width    int
	Height   int
	Alt      string
	Title    string
	Caption  string
	Credit   string
	URL      string // original for files; largest variant for images
	IsImage  bool
	Variants []VariantView
	Ready    bool // variants generated
}

type VariantView struct {
	Preset string
	Format string
	URL    string
	Width  int
	Height int
}

// Variant returns the variant for a preset (falls back to the largest).
func (v *View) Variant(preset string) *VariantView {
	var best *VariantView
	for i := range v.Variants {
		if v.Variants[i].Preset == preset {
			return &v.Variants[i]
		}
		if best == nil || v.Variants[i].Width > best.Width {
			best = &v.Variants[i]
		}
	}
	return best
}

// Srcset lists the "inside" variants (natural aspect ratio) by width.
func (v *View) Srcset() string {
	var parts []string
	seen := map[int]bool{}
	for _, vv := range v.Variants {
		if seen[vv.Width] {
			continue
		}
		if vv.Preset != "small" && vv.Preset != "medium" && vv.Preset != "large" && vv.Preset != "xl" {
			continue
		}
		seen[vv.Width] = true
		parts = append(parts, fmt.Sprintf("%s %dw", vv.URL, vv.Width))
	}
	return strings.Join(parts, ", ")
}

func (s *Service) publicURL(key string) string {
	return strings.TrimRight(s.BaseURL, "/") + "/" + strings.TrimPrefix(key, "public/")
}

// Lookup resolves ids to views in two queries (no N+1).
func (s *Service) Lookup(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*View, error) {
	out := map[uuid.UUID]*View{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Q.MediaByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	vars, err := s.DB.Q.VariantsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		v := &View{ID: m.ID, Filename: m.Filename, Mime: m.Mime, Size: m.Size, Alt: m.Alt, Title: m.Title,
			Caption: m.Caption, Credit: m.Credit, IsImage: strings.HasPrefix(m.Mime, "image/") && m.Mime != "image/svg+xml"}
		if m.Width != nil {
			v.Width, v.Height = int(*m.Width), int(*m.Height)
		}
		if !v.IsImage {
			v.URL = s.publicURL(m.StorageKey)
		}
		out[m.ID] = v
	}
	for _, vr := range vars {
		v := out[vr.MediaID]
		if v == nil {
			continue
		}
		v.Variants = append(v.Variants, VariantView{Preset: vr.Preset, Format: vr.Format, URL: s.publicURL(vr.StorageKey),
			Width: int(vr.Width), Height: int(vr.Height)})
		v.Ready = true
	}
	for _, v := range out {
		if v.IsImage {
			if best := v.Variant("xl"); best != nil {
				v.URL = best.URL
			}
		}
	}
	return out, nil
}

// Hash is a short content hash for logs.
func Hash(b []byte) string { return hex.EncodeToString(b)[:12] }
