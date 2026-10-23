package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"cms/internal/db"
	"cms/internal/forms"
	"cms/internal/jobs"
	"cms/internal/mail"
	"cms/internal/secaudit"
)

func (a *App) registerWorkers(w *river.Workers) {
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.RenderInvalidate]) error {
		return a.Renderer.Invalidate(SystemCtx(ctx, "render"), j.Args)
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.RenderRebuild]) error {
		a.Log.Info("rebuilding site", "reason", j.Args.Reason)
		_, err := a.Renderer.Rebuild(SystemCtx(ctx, "render"))
		return err
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.SchedulerSweep]) error {
		pub, unpub, err := a.Content.RunSchedule(SystemCtx(ctx, "scheduler"))
		if pub+unpub > 0 {
			a.Log.Info("scheduler", "published", pub, "unpublished", unpub)
		}
		return err
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.MediaProcess]) error {
		return a.Media.Process(ctx, j.Args)
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.Maintenance]) error {
		return a.Maintenance(ctx)
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.SearchReindex]) error {
		_, err := a.Content.Reindex(SystemCtx(ctx, "search"))
		return err
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.SecurityAudit]) error {
		return a.SecurityAudit(SystemCtx(ctx, "security"))
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.WebhookDeliver]) error {
		return a.Webhooks.Deliver(ctx, j.Args, j.Attempt)
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[forms.Notify]) error {
		return a.NotifyForm(ctx, j.Args.SubmissionID)
	}))
	river.AddWorker(w, river.WorkFunc(func(ctx context.Context, j *river.Job[jobs.WebhookEvent]) error {
		return a.Webhooks.FanOut(ctx, j.Args)
	}))
}

// Maintenance prunes old data and prepares audit partitions. Idempotent.
func (a *App) Maintenance(ctx context.Context) error {
	q := a.JobDBOrWeb().Q
	steps := []struct {
		name string
		fn   func() error
	}{
		{"audit partitions", func() error { return q.EnsureAuditPartitions(ctx) }},
		{"sessions", func() error { _, err := q.DeleteExpiredSessions(ctx, "24 hours"); return err }},
		{"auth tokens", func() error { _, err := q.DeleteExpiredAuthTokens(ctx); return err }},
		{"revisions", func() error {
			_, err := q.PruneRevisions(ctx, dbqPrune(50, "30 days"))
			return err
		}},
		{"404 log", func() error { _, err := q.PruneNotFound(ctx, "90 days"); return err }},
		{"deleted media", func() error { _, err := a.Media.PurgeDeleted(ctx, 30*24*time.Hour); return err }},
		{"webhook deliveries", func() error { _, err := q.PruneDeliveries(ctx); return err }},
		{"form submissions", func() error { _, err := a.Forms.Prune(ctx); return err }},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			a.Log.Error("maintenance step failed", "step", s.name, "err", err)
		}
	}
	return nil
}

// NotifyForm emails a form's recipients about one submission.
func (a *App) NotifyForm(ctx context.Context, id uuid.UUID) error {
	sub, err := a.Forms.Get(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return nil // deleted before the job ran
		}
		return err
	}
	admin := strings.TrimRight(a.Cfg.BaseURL, "/") + "/admin/forms/" + sub.Form + "/" + sub.ID.String()
	f, m := a.Forms.Message(sub, a.Site().Config.Name, admin)
	for _, to := range f.Notify {
		if err := a.Mail.Send(ctx, mail.Message{To: to, Subject: m.Subject, Text: m.Text, ReplyTo: m.ReplyTo}); err != nil {
			return err
		}
	}
	return nil
}

// SecurityAudit runs the self-audit and alerts owners about new serious findings.
func (a *App) SecurityAudit(ctx context.Context) error {
	findings, fresh, err := a.Security.Run(ctx)
	if err != nil {
		return err
	}
	freshSet := map[string]bool{}
	for _, id := range fresh {
		freshSet[id] = true
	}
	var lines []string
	for _, f := range findings {
		if freshSet[f.ID] && secaudit.AtLeast(f.Severity, secaudit.High) {
			lines = append(lines, "- ["+string(f.Severity)+"] "+f.Title+"\n  "+f.Remediation)
			_ = a.Jobs.Enqueue(ctx, jobs.WebhookEvent{Event: "security.finding.created", Payload: map[string]any{
				"id": f.ID, "severity": f.Severity, "title": f.Title}})
		}
	}
	if len(lines) == 0 {
		return nil
	}
	to := a.Cfg.Security.AlertEmail
	if to == "" {
		users, err := a.DB.Q.ListUsers(ctx)
		if err == nil {
			for _, u := range users {
				if u.Role == "owner" && u.Status == "active" {
					to = u.Email
					break
				}
			}
		}
	}
	if to == "" {
		return nil
	}
	return a.Mail.Send(ctx, mail.Message{To: to, Subject: "Security findings on " + a.Site().Config.Name,
		Text: "The daily security check found new issues:\n\n" + strings.Join(lines, "\n") + "\n\nSee " + a.Cfg.BaseURL + "/admin/security"})
}
