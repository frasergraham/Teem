package plan

// Stage is a finer-grained lifecycle marker on top of Status. Where
// Status answers "is this task open or done," Stage answers "which
// chunk of work is happening right now": specced → in_review →
// merging → verified. The dashboard uses Stage to render the
// pipeline-style task board; Status still gates open/closed.
type Stage string

const (
	StageProposed  Stage = "proposed"
	StageSpecced   Stage = "specced"
	StageBuilding  Stage = "building"
	StageInReview  Stage = "in_review"
	StageMerging   Stage = "merging"
	StageVerified  Stage = "verified"
	StageBlocked   Stage = "blocked"
	StageAbandoned Stage = "abandoned"
)

// AllStages enumerates the canonical stages for UI rendering and
// validation. New stages added here flow through unchanged because
// the transition matrix is closed-set checked, not open enumeration.
var AllStages = []Stage{
	StageProposed,
	StageSpecced,
	StageBuilding,
	StageInReview,
	StageMerging,
	StageVerified,
	StageBlocked,
	StageAbandoned,
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
// Conservative — re-entering a prior stage (e.g. building → specced
// for a re-spec) and dropping into blocked from anywhere are both
// permitted because real workflows backtrack. Verified and
// abandoned are terminal except for the explicit unblock paths.
var allowedTransitions = map[Stage]map[Stage]bool{
	StageProposed: {
		StageSpecced:   true,
		StageBuilding:  true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageSpecced: {
		StageProposed:  true,
		StageBuilding:  true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageBuilding: {
		StageSpecced:   true,
		StageInReview:  true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageInReview: {
		StageBuilding:  true,
		StageMerging:   true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageMerging: {
		StageInReview:  true,
		StageBuilding:  true,
		StageVerified:  true,
		StageBlocked:   true,
		StageAbandoned: true,
	},
	StageVerified: {
		// Terminal in practice; allow re-open by jumping to building
		// when a regression is found.
		StageBuilding: true,
	},
	StageBlocked: {
		// Unblock back into whichever stage was current. Callers pick.
		StageProposed:  true,
		StageSpecced:   true,
		StageBuilding:  true,
		StageInReview:  true,
		StageMerging:   true,
		StageAbandoned: true,
	},
	StageAbandoned: {
		// Reopen explicitly into building if someone changes their mind.
		StageBuilding: true,
	},
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
