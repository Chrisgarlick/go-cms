// Package forms accepts public form submissions (spec 4.8): contact forms,
// enquiries and the like, defined in a site's forms/<name>.yaml.
//
// Pages with forms stay cacheable. The form fetches a short-lived signed token
// from /forms/<name>/token when the visitor first interacts with it; the token
// also proves a minimum time-to-submit. A honeypot field and a per-IP limit
// keep out the rest. Values are validated with the same field definitions as
// content, stored encrypted, and emailed to the form's recipients by a job.
package forms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"cms/internal/audit"
	"cms/internal/auth"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/keys"
	"cms/internal/site"
)

// Honeypot is the name of the hidden field that must arrive empty.
const Honeypot = "_hp"

var (
	ErrNoForm      = errors.New("forms: no such form")
	ErrToken       = errors.New("This form has expired. Please reload the page and try again.") //nolint:staticcheck // shown to visitors as-is
	ErrTooFast     = errors.New("That was quicker than a person can type. Please try again.")   //nolint:staticcheck // shown to visitors as-is
	ErrRateLimited = errors.New("Too many submissions. Please try again later.")                //nolint:staticcheck // shown to visitors as-is
	ErrSpam        = errors.New("forms: honeypot filled")
)

const (
	minAge = 2 * time.Second
	maxAge = 12 * time.Hour
)

// Notify is the job that emails a form's recipients about one submission.
type Notify struct {
	SubmissionID uuid.UUID `json:"submission_id"`
}

func (Notify) Kind() string { return "form.notify" }
func (Notify) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10, Queue: river.QueueDefault}
}

type Enqueuer interface {
	Enqueue(ctx context.Context, args ...river.JobArgs) error
}

type Service struct {
	DB   *db.DB
	Keys *keys.Keyring
	Jobs Enqueuer
	Site func() *site.Bundle
	Log  *slog.Logger
	// Now is overridable in tests.
	Now func() time.Time

	limit *auth.Limiter
}

func New(d *db.DB, kr *keys.Keyring, j Enqueuer, b func() *site.Bundle, log *slog.Logger) *Service {
	return &Service{DB: d, Keys: kr, Jobs: j, Site: b, Log: log, Now: time.Now, limit: auth.NewLimiter(5, 10*time.Minute)}
}

func (s *Service) Form(name string) *site.Form {
	if b := s.Site(); b != nil {
		return b.Forms[name]
	}
	return nil
}

// Token issues a token for one form: "<unix seconds>.<hmac>".
func (s *Service) Token(form string) string {
	ts := strconv.FormatInt(s.Now().Unix(), 10)
	return ts + "." + s.sign(form, ts)
}

func (s *Service) sign(form, ts string) string {
	m := hmac.New(sha256.New, s.Keys.Derive("forms"))
	m.Write([]byte(form + "\x00" + ts))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) checkToken(form, tok string) error {
	ts, sig, ok := strings.Cut(tok, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(form, ts))) {
		return ErrToken
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrToken
	}
	age := s.Now().Sub(time.Unix(n, 0))
	switch {
	case age > maxAge || age < -time.Minute:
		return ErrToken
	case age < minAge:
		return ErrTooFast
	}
	return nil
}

// Submission is one decrypted submission.
type Submission struct {
	ID      uuid.UUID
	Form    string
	Values  map[string]any
	Summary string
	Created time.Time
	Read    bool
}

// Submit validates and stores a submission, then queues the notification.
// Validation failures are returned as fields.Errors (field name → message).
func (s *Service) Submit(ctx context.Context, name string, values map[string]any, token, honeypot string, ip netip.Addr) (*site.Form, error) {
	f := s.Form(name)
	if f == nil {
		return nil, ErrNoForm
	}
	if honeypot != "" {
		return f, ErrSpam
	}
	if err := s.checkToken(name, token); err != nil {
		return f, err
	}
	if !s.limit.Allow(name + "|" + ip.String()) {
		return f, ErrRateLimited
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return f, err
	}
	clean, _, err := fields.Validate(f.Fields, raw, fields.Context{CanUse: func(string) bool { return false }})
	if err != nil {
		return f, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return f, err
	}
	enc, err := s.Keys.Encrypt(clean, aad(id))
	if err != nil {
		return f, err
	}
	var vals map[string]any
	_ = json.Unmarshal(clean, &vals)
	if err := s.DB.Q.InsertFormSubmission(ctx, dbq.InsertFormSubmissionParams{ID: id, Form: name, DataEnc: enc, Summary: summary(f, vals)}); err != nil {
		return f, err
	}
	if len(f.Notify) > 0 && s.Jobs != nil {
		if err := s.Jobs.Enqueue(ctx, Notify{SubmissionID: id}); err != nil {
			s.Log.Error("form notification not queued", "form", name, "id", id, "err", err)
		}
	}
	return f, nil
}

func aad(id uuid.UUID) []byte { return []byte("form_submission:" + id.String()) }

// summary is a short, non-sensitive line for lists: the first short text field
// (normally a name). Email addresses and long text are left out.
func summary(f *site.Form, vals map[string]any) string {
	for _, fd := range f.Fields {
		if fd.Type != fields.Text {
			continue
		}
		if v, ok := vals[fd.Name].(string); ok && strings.TrimSpace(v) != "" {
			v = strings.TrimSpace(v)
			if r := []rune(v); len(r) > 60 {
				v = string(r[:60]) + "…"
			}
			return v
		}
	}
	return ""
}

func (s *Service) decrypt(row dbq.FormSubmission) (*Submission, error) {
	plain, err := s.Keys.Decrypt(row.DataEnc, aad(row.ID))
	if err != nil {
		return nil, err
	}
	sub := &Submission{ID: row.ID, Form: row.Form, Summary: row.Summary, Created: row.CreatedAt, Read: row.ReadAt != nil}
	if err := json.Unmarshal(plain, &sub.Values); err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Submission, error) {
	row, err := s.DB.Q.GetFormSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.decrypt(row)
}

// List returns one page of a form's submissions (newest first), decrypted.
func (s *Service) List(ctx context.Context, form string, limit, offset int) ([]*Submission, error) {
	rows, err := s.DB.Q.ListFormSubmissions(ctx, dbq.ListFormSubmissionsParams{Form: form, Limit: int32(limit), Offset: int32(offset)})
	if err != nil {
		return nil, err
	}
	out := make([]*Submission, 0, len(rows))
	for _, r := range rows {
		sub, err := s.decrypt(r)
		if err != nil {
			s.Log.Error("form submission cannot be decrypted", "id", r.ID, "err", err)
			continue
		}
		out = append(out, sub)
	}
	return out, nil
}

type Count struct{ Total, Unread int64 }

func (s *Service) Counts(ctx context.Context) (map[string]Count, error) {
	rows, err := s.DB.Q.FormSubmissionCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Count{}
	for _, r := range rows {
		out[r.Form] = Count{Total: r.Total, Unread: r.Unread}
	}
	return out, nil
}

func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	return s.DB.Q.MarkFormSubmissionRead(ctx, id)
}

// Delete removes a submission (audited; the actor comes from ctx).
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		row, err := q.GetFormSubmission(ctx, id)
		if err != nil {
			return err
		}
		if _, err := q.DeleteFormSubmission(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "form.submission.delete", ResourceType: "form_submission", ResourceID: id.String(),
			Diff: map[string]any{"form": row.Form}})
	})
}

// Prune deletes submissions past each form's retention period.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	var total int64
	b := s.Site()
	if b == nil {
		return 0, nil
	}
	for name, f := range b.Forms {
		n, err := s.DB.Q.PruneFormSubmissions(ctx, dbq.PruneFormSubmissionsParams{Form: name, Age: fmt.Sprintf("%d days", f.RetentionDays)})
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Message renders the notification email for a submission.
type Message struct {
	Subject string
	Text    string
	ReplyTo string
}

func (s *Service) Message(sub *Submission, siteName, adminURL string) (*site.Form, Message) {
	f := s.Form(sub.Form)
	if f == nil {
		f = &site.Form{Name: sub.Form, Label: sub.Form, Subject: "New {label} submission"}
	}
	subject := strings.ReplaceAll(f.Subject, "{label}", f.Label)
	var b strings.Builder
	fmt.Fprintf(&b, "New %s submission on %s\n\n", f.Label, siteName)
	seen := map[string]bool{}
	write := func(label string, v any) {
		val := Format(v)
		if strings.Contains(val, "\n") {
			fmt.Fprintf(&b, "%s:\n%s\n\n", label, val)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", label, val)
		}
	}
	for _, fd := range f.Fields {
		seen[fd.Name] = true
		label := fd.Label
		if label == "" {
			label = fields.Humanize(fd.Name)
		}
		write(label, sub.Values[fd.Name])
		subject = strings.ReplaceAll(subject, "{"+fd.Name+"}", oneLine(Format(sub.Values[fd.Name])))
	}
	var extra []string
	for k := range sub.Values {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		write(fields.Humanize(k), sub.Values[k])
	}
	fmt.Fprintf(&b, "\nSent %s\nView it in the admin: %s\n", sub.Created.Format("2 Jan 2006 15:04 MST"), adminURL)
	m := Message{Subject: oneLine(subject), Text: b.String()}
	if f.ReplyTo != "" {
		if v, ok := sub.Values[f.ReplyTo].(string); ok {
			m.ReplyTo = oneLine(v)
		}
	}
	return f, m
}

// Format renders a stored value as text.
func Format(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case string:
		if t == "" {
			return "—"
		}
		return t
	case bool:
		if t {
			return "Yes"
		}
		return "No"
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = Format(x)
		}
		return strings.Join(parts, ", ")
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 150 {
		s = string(r[:150])
	}
	return s
}
