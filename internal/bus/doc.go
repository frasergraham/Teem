// Package bus is the message-passing fabric between the orchestrator,
// the Leader, and worker agents.
//
// The v1 implementation is in-process Go channels (MemBus). The interface
// is shaped so a tailnet-backed or file-backed bus can be slotted in later
// without changing callers.
package bus
