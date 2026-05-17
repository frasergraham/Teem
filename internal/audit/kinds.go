package audit

// AllKinds is the closed registry of every Kind constant defined in this
// package. Code that branches on Kind (the pulse audit-nudge gate, the
// SPA's audit_patches handlers map) iterates this list to assert
// coverage at test time — every Kind here MUST be classified explicitly,
// so adding a new constant fails the lint test until the consumer is
// updated.
//
// The kind field on Event is still a string (callers may emit ad-hoc
// kinds for one-off events); IsRegistered tells you whether a given
// value corresponds to a constant in this file.
var AllKinds = []Kind{
	KindJobReceived,
	KindJobComplete,
	KindJobError,
	KindNote,
	KindHeartbeat,
	KindJobInterrupted,
	KindJobTranscriptReady,
	KindWorkerStopped,
	KindDecisionNote,
	KindBlockerNote,
	KindTaskStageChanged,
	KindPMTick,
	KindChannelsState,
	KindUsageEvent,
	KindUsageThrottle,
	KindPulseTick,
	KindLeaderChatTurn,
	KindTaskCreated,
	KindLeaderStatusChanged,
}

var registeredKinds = func() map[Kind]struct{} {
	m := make(map[Kind]struct{}, len(AllKinds))
	for _, k := range AllKinds {
		m[k] = struct{}{}
	}
	return m
}()

// IsRegistered reports whether k is one of the Kind constants declared
// in this package (i.e. present in AllKinds). Returns false for ad-hoc
// kinds emitted as raw strings.
func IsRegistered(k Kind) bool {
	_, ok := registeredKinds[k]
	return ok
}
