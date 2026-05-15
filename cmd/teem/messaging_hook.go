package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/messaging"
)

// messagingDeliveryTimeout caps the per-event Notify call so a slow
// Telegram API never blocks the audit writer goroutine. The hook
// otherwise mirrors makeChannelHook in shape.
const messagingDeliveryTimeout = 5 * time.Second

// makeMessagingHook returns an auditHook that filters the audit stream
// to the operator-must-see subset, lowers each event through fmtr, and
// hands the rendered Message to n. Dedup drops repeats inside the
// configured window. tokens, when non-nil, stamps each outbound Message
// with a freshly-issued reply token so the operator can /reply on
// Telegram and reach the inbound webhook handler. Any of n / fmtr /
// dedup being nil yields a nil hook (no-op so combineHooks drops it).
func makeMessagingHook(n messaging.Notifier, fmtr messaging.MessageFormatter, dedup *messaging.Dedup, tokens *messaging.ReplyTokenStore) auditHook {
	if n == nil || dedup == nil {
		return nil
	}
	return func(events []audit.Event) {
		for _, e := range events {
			msg, ok := fmtr.Format(e)
			if !ok {
				continue
			}
			if !dedup.Allow(msg) {
				continue
			}
			if tokens != nil {
				tok, err := tokens.Issue(messaging.ReplyContext{
					TeamID:  msg.TeamID,
					TaskID:  msg.TaskID,
					AgentID: msg.AgentID,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "[messaging] issue reply token: %v\n", err)
				} else {
					msg.ReplyToken = tok
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), messagingDeliveryTimeout)
			if err := n.Notify(ctx, msg); err != nil {
				fmt.Fprintf(os.Stderr, "[messaging] notify failed: %v\n", err)
			}
			cancel()
		}
	}
}
