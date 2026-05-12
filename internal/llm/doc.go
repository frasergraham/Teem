// Package llm is a backend-agnostic client interface for one-shot LLM
// completions used by Teem's utility code paths.
//
// Agents themselves talk to Claude through the `claude` CLI; this package
// is for everything else (small reasoning helpers, summaries, etc.). The
// v1 ships an Anthropic implementation and is exercised through the
// `teem llm ping` subcommand.
package llm
