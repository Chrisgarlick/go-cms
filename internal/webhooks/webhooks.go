// Package webhooks delivers signed event notifications (spec 4.16).
// Signature: X-CMS-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">.
// Receivers should reject timestamps older than five minutes.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/keys"
	"cms/internal/netx"
	"cms/internal/rbac"
)

// Events a webhook can subscribe to ("*" for all).
var Events = []string{"entry.published", "entry.unpublished", "entry.trashed", "media.uploaded", "security.finding.created"}

type Service struct {
	DB     *db.DB
	Keys   *keys.Keyring
	Jobs   jobs.Enqueuer
	Client *http.Client
}

func aad(id uuid.UUID) []byte { return []byte("webhooks:" + id.String() + ":secret") }

// Create registers a webhook and returns its signing secret (shown once).
func (s *Service) Create(ctx context.Context, rawURL string, events []string) (dbq.Webhook, string, error) {
	if _, err := actor.RequireSudo(ctx, rbac.WebhooksManage); err != nil {
		return dbq.Webhook{}, "", err
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return dbq.Webhook{}, "", errors.New("webhook URL must be https://")
	}
	if len(events) == 0 {
		events = []string{"*"}
	}
	for _, e := range events {
		ok := e == "*"
		for _, k := range Events {
			ok = ok || e == k
		}
		if !ok {
			return dbq.Webhook{}, "", fmt.Errorf("unknown event %q", e)
		}
	}
	id := uuid.Must(uuid.NewV7())
	secret := "whsec_" + randomHex(24)
	enc, err := s.Keys.Encrypt([]byte(secret), aad(id))
	if err != nil {
		return dbq.Webhook{}, "", err
	}
	var w dbq.Webhook
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		w, err = q.CreateWebhook(ctx, dbq.CreateWebhookParams{ID: id, Url: u.String(), SecretEnc: enc, Events: events})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "webhook.create", ResourceType: "webhook", ResourceID: id.String(),
			Diff: map[string]any{"host": u.Host, "events": events}})
	})
	return w, secret, err
}

func (s *Service) List(ctx context.Context) ([]dbq.Webhook, error) {
	if _, err := actor.Require(ctx, rbac.WebhooksManage); err != nil {
		return nil, err
	}
	return s.DB.Q.ListWebhooks(ctx)
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.WebhooksManage); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.DeleteWebhook(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "webhook.delete", ResourceType: "webhook", ResourceID: id.String()})
	})
}

func (s *Service) Deliveries(ctx context.Context, id uuid.UUID) ([]dbq.WebhookDelivery, error) {
	if _, err := actor.Require(ctx, rbac.WebhooksManage); err != nil {
		return nil, err
	}
	return s.DB.Q.ListDeliveries(ctx, id)
}

// FanOut enqueues one delivery job per subscribed webhook.
func (s *Service) FanOut(ctx context.Context, ev jobs.WebhookEvent) error {
	hooks, err := s.DB.Q.WebhooksFor(ctx, ev.Event)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		did := uuid.Must(uuid.NewV7())
		if err := s.DB.Q.CreateDelivery(ctx, dbq.CreateDeliveryParams{ID: did, WebhookID: h.ID, Event: ev.Event}); err != nil {
			return err
		}
		if err := s.Jobs.Enqueue(ctx, jobs.WebhookDeliver{WebhookID: h.ID.String(), DeliveryID: did.String(), Event: ev.Event, Payload: ev.Payload}); err != nil {
			return err
		}
	}
	return nil
}

// Deliver posts one event. Errors make River retry with backoff.
func (s *Service) Deliver(ctx context.Context, args jobs.WebhookDeliver, attempt int) error {
	id, err := uuid.Parse(args.WebhookID)
	if err != nil {
		return nil
	}
	h, err := s.DB.Q.GetWebhook(ctx, id)
	if err != nil || !h.Active {
		return nil // deleted or disabled since enqueue
	}
	secret, err := s.Keys.Decrypt(h.SecretEnc, aad(h.ID))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"id": args.DeliveryID, "event": args.Event, "created_at": time.Now().UTC(), "data": args.Payload})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	sig := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.Url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cms-webhooks/1")
	req.Header.Set("X-CMS-Event", args.Event)
	req.Header.Set("X-CMS-Delivery", args.DeliveryID)
	req.Header.Set("X-CMS-Signature", sig)
	client := s.Client
	if client == nil {
		client = netx.Client(15 * time.Second)
	}
	resp, err := client.Do(req)
	status := int32(0)
	note := ""
	if err != nil {
		note = err.Error()
	} else {
		status = int32(resp.StatusCode)
		b, _ := netx.ReadLimited(resp.Body, 1024)
		resp.Body.Close()
		note = string(b)
	}
	if did, perr := uuid.Parse(args.DeliveryID); perr == nil {
		_ = s.DB.Q.UpdateDelivery(ctx, dbq.UpdateDeliveryParams{ID: did, Status: &status, Attempts: int32(attempt), Response: note})
	}
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("webhook %s returned %d", h.ID, status)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = cryptoRand(b)
	return hex.EncodeToString(b)
}
