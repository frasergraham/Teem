package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// safeGo runs fn in a new goroutine with a top-level recover. A panic
// inside fn is logged with its full stack and swallowed — the daemon
// keeps running instead of crashing the whole process. Use this for
// background goroutines whose failure should not take down the daemon
// (summarizer ticks, reconcile passes, retention sweeps, etc).
//
// Do NOT use safeGo for goroutines whose failure should be fatal — e.g.
// the HTTP serve loop. There, an unhandled panic is the right signal.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[teemd] PANIC in goroutine %q: %v\n%s\n", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// safeCall runs fn inline with the same recover behaviour. Useful for
// HTTP handler hot paths and per-tick callbacks where a panic in one
// iteration should not kill the loop or the connection.
func safeCall(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[teemd] PANIC in %q: %v\n%s\n", name, r, debug.Stack())
		}
	}()
	fn()
}
