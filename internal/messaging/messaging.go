// Package messaging delivers operator-must-see audit events to external
// messaging channels (Telegram in v1; WhatsApp/Slack/Discord deferred).
// See docs/messaging-integration.md.
//
// Notifier is the channel-agnostic delivery interface. MessageFormatter
// (format.go) is the single place that lowers audit.Event into a Message;
// notifiers ferry the rendered Message and pick which fields to surface.
package messaging

import (
	"context"
	"errors"
)

// Severity tags a Message so the formatter and any channel-specific
// rendering can choose icons / priority. Closed enum: a new severity
// is a deliberate design decision, not a free-form string.
type Severity string

const (
	SeverityInfo     Severity = "info"     // FYI: no operator action required
	SeverityDecision Severity = "decision" // record_decision question — operator should glance
	SeverityWarning  Severity = "warning"  // blocker — operator should look soon
	SeverityAction   Severity = "action"   // awaiting_approval — operator must act
)

// Message is the channel-agnostic payload. A Notifier picks which
// fields to render.
type Message struct {
	Title      string
	Summary    string
	Severity   Severity
	Link       string
	ReplyToken string
	TaskID     string
	AgentID    string
	TeamID     string
}

// Notifier delivers a Message to a channel. Implementations should
// respect ctx cancellation but treat delivery failure as best-effort —
// log and drop; don't crash the daemon, don't retry forever.
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
}

// MultiNotifier fans a Message out to every wrapped Notifier and
// collects any errors. Empty slice / nil is a valid no-op.
type MultiNotifier []Notifier

func (m MultiNotifier) Notify(ctx context.Context, msg Message) error {
	var errs []error
	for _, n := range m {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
