package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleAPITeamRoute dispatches /api/teams/<id>/... routes.
//
//	GET /api/teams/<id>/state   → full snapshot JSON payload (see
//	                              apiTeamStatePayload below). Phase 2b
//	                              ports dashboardTeam to a JSON
//	                              projection; the SPA reads this on
//	                              page load.
//	GET /api/teams/<id>/events  → WebSocket delta stream.
//
// Auth model matches the dashboard's tailnet boundary: no bearer
// required.
func (d *daemon) handleAPITeamRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/teams/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	id, suffix := rest[:slash], rest[slash:]
	rt := d.resolveTeam(id)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	switch suffix {
	case "/state":
		d.handleAPITeamState(w, r, rt)
	case "/events":
		d.handleAPITeamEvents(w, r, rt)
	default:
		http.NotFound(w, r)
	}
}

// apiTeamMeta is the {team:{...}} subobject. Kept small so the SPA can
// render a header without walking the full payload.
type apiTeamMeta struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RegisteredAgo string `json:"registered_ago"`
}

// apiTaskBuckets groups the four dashboardTask slices under a single
// `tasks` key so the SPA store can fan them out cleanly. Matches the
// shape sketched in docs/dashboard-spa.md §6.
type apiTaskBuckets struct {
	Open             []dashboardTask        `json:"open"`
	AwaitingApproval []awaitingApprovalTask `json:"awaiting_approval"`
	Shelved          []dashboardTask        `json:"shelved"`
	RecentDone       []dashboardTask        `json:"recent_done"`
}

// apiTeamStatePayload is the JSON projection of dashboardTeam plus
// Now/ETag at the top. Built from teamSnapshot so SSR and SPA stay in
// lockstep — every change to the dashboardTeam shape is automatically
// reflected here.
type apiTeamStatePayload struct {
	Now              time.Time        `json:"now"`
	ETag             string           `json:"etag"`
	Team             apiTeamMeta      `json:"team"`
	Hero             teamHero         `json:"hero"`
	Agents           []dashboardAgent `json:"agents"`
	Workers          []workerRow      `json:"workers"`
	Tasks            apiTaskBuckets   `json:"tasks"`
	OpenTaskCount    int              `json:"open_task_count"`
	Decisions        []decisionRow    `json:"decisions"`
	LeaderStatus     *leaderRow       `json:"leader_status"`
	OtherStatuses    []leaderRow      `json:"other_statuses"`
	Pulse            pulseSnapshot    `json:"pulse"`
	Usage            *usageSnapshot   `json:"usage"`
	Branches         teamPageBranches `json:"branches"`
	ChannelsState    string           `json:"channels_state"`
	StatusHeadline   string           `json:"status_headline"`
	RecentEvents     []dashboardEvent `json:"recent_events"`
	InFlight         int64            `json:"in_flight"`
	UnreadNotes      int              `json:"unread_notes"`
	HasRepo          bool             `json:"has_repo"`
	HasPricing       bool             `json:"has_pricing"`
	PricingStale     bool             `json:"pricing_stale"`
	HeroSpendUSD     float64          `json:"hero_spend_usd"`
	HeroSpendDisplay string           `json:"hero_spend_display"`
}

func (d *daemon) handleAPITeamState(w http.ResponseWriter, _ *http.Request, rt *registeredTeam) {
	tv := d.snapshotTeam(rt)
	team := teamSnapshot(tv)
	team.Usage = buildUsageSnapshot(d.usageAgg, time.Now())

	rt.detectionMu.Lock()
	channelsLive := rt.channelsLive
	rt.detectionMu.Unlock()
	channelsState := "fallback"
	if channelsLive {
		channelsState = "live"
	}

	payload := apiTeamStatePayload{
		Now: time.Now().UTC(),
		Team: apiTeamMeta{
			ID:            team.ID,
			Name:          team.Name,
			RegisteredAgo: team.RegisteredAgo,
		},
		Hero:    team.Hero,
		Agents:  team.Agents,
		Workers: team.Workers,
		Tasks: apiTaskBuckets{
			Open:             team.OpenTasks,
			AwaitingApproval: team.AwaitingApproval,
			Shelved:          team.Shelved,
			RecentDone:       team.RecentDone,
		},
		OpenTaskCount:    team.OpenTaskCount,
		Decisions:        team.Decisions,
		LeaderStatus:     team.LeaderStatus,
		OtherStatuses:    team.OtherStatuses,
		Pulse:            team.Pulse,
		Usage:            team.Usage,
		Branches:         team.Branches,
		ChannelsState:    channelsState,
		StatusHeadline:   team.StatusHeadline,
		RecentEvents:     team.RecentEvents,
		InFlight:         team.InFlight,
		UnreadNotes:      team.UnreadNotes,
		HasRepo:          team.HasRepo,
		HasPricing:       team.HasPricing,
		PricingStale:     team.PricingStale,
		HeroSpendUSD:     team.HeroSpendUSD,
		HeroSpendDisplay: team.HeroSpendDisplay,
	}

	// ETag is sha256 of the content-bearing portion (everything except
	// the etag field itself). Marshal once with etag empty, hash that,
	// then marshal again with the etag set. Clients use it for
	// reconnect change-detection (see docs/dashboard-spa.md §7); the
	// daemon never inspects If-None-Match in Phase 2b.
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	payload.ETag = "sha256:" + hex.EncodeToString(sum[:])

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}
