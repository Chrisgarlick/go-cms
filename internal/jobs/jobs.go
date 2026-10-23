// Package jobs defines background job arguments and the enqueue interface.
// Workers are registered by the app wiring, where every service exists; this
// package stays dependency-free so services can enqueue without import cycles.
// Every job must be idempotent: during a deploy it may run on both the old and
// the new worker (spec 4.15).
package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const (
	QueueDefault = river.QueueDefault
	QueueRender  = "render"
	QueueMedia   = "media"
)

// Dep identifies something a rendered page read.
type Dep struct {
	Type string `json:"type"` // entry | type | term | menu | options | settings | site
	ID   string `json:"id"`
}

// RenderInvalidate deletes and re-renders pages that depend on Deps, plus Paths.
type RenderInvalidate struct {
	Deps  []Dep    `json:"deps"`
	Paths []string `json:"paths"`
}

func (RenderInvalidate) Kind() string { return "render.invalidate" }
func (RenderInvalidate) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueRender, MaxAttempts: 5}
}

// RenderRebuild re-renders the whole site (theme, bundle or settings change).
type RenderRebuild struct {
	Reason string `json:"reason"`
}

func (RenderRebuild) Kind() string { return "render.rebuild" }
func (RenderRebuild) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueRender, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: 30 * time.Second}}
}

// SchedulerSweep publishes and unpublishes due entries.
type SchedulerSweep struct{}

func (SchedulerSweep) Kind() string { return "scheduler.sweep" }
func (SchedulerSweep) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: 50 * time.Second}}
}

// MediaProcess generates image variants.
type MediaProcess struct {
	MediaID string `json:"media_id"`
	Force   bool   `json:"force"`
}

func (MediaProcess) Kind() string { return "media.process" }
func (MediaProcess) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMedia, MaxAttempts: 3}
}

// WebhookDeliver posts one event to one webhook.
type WebhookDeliver struct {
	WebhookID  string         `json:"webhook_id"`
	DeliveryID string         `json:"delivery_id"`
	Event      string         `json:"event"`
	Payload    map[string]any `json:"payload"`
}

func (WebhookDeliver) Kind() string                 { return "webhook.deliver" }
func (WebhookDeliver) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: 8} }

// Maintenance prunes revisions, sessions, tokens, logs and creates audit partitions.
type Maintenance struct{}

func (Maintenance) Kind() string { return "maintenance" }
func (Maintenance) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: 30 * time.Minute}}
}

// SecurityAudit runs the self-audit (spec 4.20).
type SecurityAudit struct{}

func (SecurityAudit) Kind() string { return "security.audit" }
func (SecurityAudit) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}
}

// SearchReindex rebuilds the search vectors of every published entry.
type SearchReindex struct{}

func (SearchReindex) Kind() string { return "search.reindex" }

// Enqueuer inserts jobs inside the caller's transaction, so a job exists if
// and only if the change that caused it committed.
type Enqueuer interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, args ...river.JobArgs) error
	Enqueue(ctx context.Context, args ...river.JobArgs) error
}

// RiverEnqueuer adapts a River client.
type RiverEnqueuer struct{ Client *river.Client[pgx.Tx] }

func (r RiverEnqueuer) EnqueueTx(ctx context.Context, tx pgx.Tx, args ...river.JobArgs) error {
	for _, a := range args {
		if _, err := r.Client.InsertTx(ctx, tx, a, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r RiverEnqueuer) Enqueue(ctx context.Context, args ...river.JobArgs) error {
	for _, a := range args {
		if _, err := r.Client.Insert(ctx, a, nil); err != nil {
			return err
		}
	}
	return nil
}

// NewInsertOnly returns a client that can enqueue but runs no workers (web process).
func NewInsertOnly(pool riverpgxv5Pool) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{})
}

// Discard is an Enqueuer for tests that drops every job.
type Discard struct{ Jobs []river.JobArgs }

func (d *Discard) EnqueueTx(_ context.Context, _ pgx.Tx, args ...river.JobArgs) error {
	d.Jobs = append(d.Jobs, args...)
	return nil
}

func (d *Discard) Enqueue(_ context.Context, args ...river.JobArgs) error {
	d.Jobs = append(d.Jobs, args...)
	return nil
}
