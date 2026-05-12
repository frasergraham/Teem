// Package transport abstracts how Teem starts a subprocess.
//
// The Leader and every Worker run `claude` as a subprocess, but where that
// subprocess lives differs by deployment: locally for dev, over SSH for a
// remote workstation, and (later) in a Railway container reachable via
// tailnet. All three look the same to the caller — start a Command, get a
// Process with stdin/stdout/stderr — via the Transport interface here.
package transport
