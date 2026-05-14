package plan

// Stage is a finer-grained lifecycle marker on top of Status. Where
// Status answers "is this task open or done," Stage answers "which
// chunk of work is happening right now": specced → planning →
// coding → reviewing → integrating → verified. The dashboard uses
// Stage to render the pipeline-style task board; Status still gates
// open/closed.
type Stage string

const (
	StageProposed Stage = "proposed"
	StageSpecced  Stage = "specced"
	// StagePlanning is the "thinking about how" sub-stage that comes
	// before code is written; split out of the old `building` stage so
	// the dashboard can distinguish a worker still designing from one
	// already typing.
	StagePlanning Stage = "planning"
	// StageCoding replaces the old `building` stage value and is the
	// catch-all "code is being written" pipeline cell.
	StageCoding      Stage = "coding"
	StageReviewing   Stage = "reviewing"
	StageIntegrating Stage = "integrating"
	StageVerified    Stage = "verified"
	StageBlocked     Stage = "blocked"
	StageAbandoned   Stage = "abandoned"
	// StageShelved pairs with StatusShelved: an explicit "paused for
	// later" pipeline cell, distinct from blocked (stuck on something)
	// or abandoned (won't do).
	StageShelved Stage = "shelved"
)

// AllStages enumerates the canonical stages for UI rendering and
// validation. New stages added here flow through unchanged because
// the transition matrix is closed-set checked, not open enumeration.
var AllStages = []Stage{
	StageProposed,
	StageSpecced,
	StagePlanning,
	StageCoding,
	StageReviewing,
	StageIntegrating,
	StageVerified,
	StageBlocked,
	StageShelved,
	StageAbandoned,
}

// stageAliases maps legacy stage strings (read off disk or accepted on
// input) to their post-rename canonical values. Keeps old plan.jsonl
// entries working after the building→planning/coding,
// in_review→reviewing, merging→integrating rename. "building" is lossy
// — most existing tasks were coding rather than planning — so we snap
// it to coding.
var stageAliases = map[string]Stage{
	"building":  StageCoding,
	"in_review": StageReviewing,
	"merging":   StageIntegrating,
	// Placeholder: worker-zane is adding "awaiting_approval" as a new
	// stage value in a separate task; do not add it here. Once landed,
	// it will need its own entry in AllStages and the transition matrix
	// rather than an alias.
}

// NormalizeStage canonicalises a stage string read off disk or
// supplied by the operator: aliases (the pre-rename names) are mapped
// to their new values; everything else is returned untouched. Use
// before validating with IsValidStage.
func NormalizeStage(s Stage) Stage {
	if alias, ok := stageAliases[string(s)]; ok {
		return alias
	}
	return s
}

// IsValidStage reports whether s is one of the known stages.
func IsValidStage(s Stage) bool {
	for _, k := range AllStages {
		if s == k {
			return true
		}
	}
	return false
}

// allowedTransitions encodes the directed graph of stage moves.
// Conservative — re-entering a prior stage (e.g. coding → specced
// for a re-spec) and dropping into blocked from anywhere are both
// permitted because real workflows backtrack. Verified and
// abandoned are terminal except for the explicit unblock paths.
var allowedTransitions = map[Stage]map[Stage]bool{
	StageProposed: {
		StageSpecced:   true,
		StagePlanning:  true,
		StageCoding:    true,
		StageBlocked:   true,
		StageShelved:   true,
		StageAbandoned: true,
	},
	StageSpecced: {
		StageProposed:  true,
		StagePlanning:  true,
		StageCoding:    true,
		StageBlocked:   true,
		StageShelved:   true,
		StageAbandoned: true,
	},
	StagePlanning: {
		StageSpecced:   true,
		StageCoding:    true,
		StageReviewing: true,
		StageBlocked:   true,
		StageShelved:   true,
		StageAbandoned: true,
	},
	StageCoding: {
		StageSpecced:   true,
		StagePlanning:  true,
		StageReviewing: true,
		StageBlocked:   true,
		StageShelved:   true,
		StageAbandoned: true,
	},
	StageReviewing: {
		StagePlanning:    true,
		StageCoding:      true,
		StageIntegrating: true,
		StageBlocked:     true,
		StageShelved:     true,
		StageAbandoned:   true,
	},
	StageIntegrating: {
		StageReviewing: true,
		StageCoding:    true,
		StageVerified:  true,
		StageBlocked:   true,
		StageShelved:   true,
		StageAbandoned: true,
	},
	StageVerified: {
		// Terminal in practice; allow re-open by jumping back to coding
		// when a regression is found.
		StageCoding: true,
	},
	StageBlocked: {
		// Unblock back into whichever stage was current. Callers pick.
		StageProposed:    true,
		StageSpecced:     true,
		StagePlanning:    true,
		StageCoding:      true,
		StageReviewing:   true,
		StageIntegrating: true,
		StageShelved:     true,
		StageAbandoned:   true,
	},
	StageShelved: {
		// Coming off the shelf — operator picks where to resume.
		StageProposed:  true,
		StageSpecced:   true,
		StagePlanning:  true,
		StageCoding:    true,
		StageReviewing: true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageAbandoned: {
		// Reopen explicitly into coding if someone changes their mind.
		StageCoding: true,
	},
	// Placeholder: worker-zane's `awaiting_approval` stage will need its
	// own row here once it lands, plus entries pointing into it from
	// whichever stages can request approval.
}

// CanTransition reports whether moving from→to is permitted. An empty
// from (task without a stage yet) accepts any valid stage as the
// initial transition.
func CanTransition(from, to Stage) bool {
	if !IsValidStage(to) {
		return false
	}
	if from == "" {
		return true
	}
	if !IsValidStage(from) {
		return false
	}
	return allowedTransitions[from][to]
}
