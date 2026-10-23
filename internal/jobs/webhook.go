package jobs

import "github.com/riverqueue/river"

// WebhookEvent fans one event out to every subscribed webhook.
type WebhookEvent struct {
	Event   string         `json:"event"`
	Payload map[string]any `json:"payload"`
}

func (WebhookEvent) Kind() string                 { return "webhook.event" }
func (WebhookEvent) InsertOpts() river.InsertOpts { return river.InsertOpts{MaxAttempts: 3} }
