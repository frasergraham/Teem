package main

import (
	"context"
	"fmt"
	"os"
	"sync"
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
//
// Idle-pulse coalescing: a pulse_tick with zero tool_calls is "idle".
// We forward the first idle in a streak but suppress every subsequent
// one — until either a non-idle pulse_tick or any other forwarded
// operator-must-see event resets the marker. Idle is idle; nagging
// the operator again adds no information.
func makeMessagingHook(n messaging.Notifier, fmtr messaging.MessageFormatter, dedup *messaging.Dedup, tokens *messaging.ReplyTokenStore) auditHook {
	if n == nil || dedup == nil {
		return nil
	}
	var (
		mu      sync.Mutex
		wasIdle bool
	)
	return func(events []audit.Event) {
		for _, e := range events {
			msg, ok := fmtr.Format(e)
			if !ok {
				continue
			}
			if e.Kind == audit.KindPulseTick {
				idle := isIdlePulseTick(e)
				mu.Lock()
				suppress := idle && wasIdle
				wasIdle = idle
				mu.Unlock()
				if suppress {
					continue
				}
			} else {
				mu.Lock()
				wasIdle = false
				mu.Unlock()
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

// isIdlePulseTick reports whether a KindPulseTick event represents an
// idle leader turn (no tool calls). The pulse loop records tool_calls
// in Meta as int; we tolerate the JSON-roundtrip float64 form too in
// case the event came back through a sink that serialised it.
func isIdlePulseTick(e audit.Event) bool {
	if e.Kind != audit.KindPulseTick {
		return false
	}
	switch v := e.Meta["tool_calls"].(type) {
	case int:
		return v == 0
	case int64:
		return v == 0
	case float64:
		return v == 0
	}
	// Missing tool_calls meta — treat as idle so an under-instrumented
	// emitter can't pin the channel busy forever.
	return true
}
