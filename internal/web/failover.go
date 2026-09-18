/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package web

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"vmsync-ui/internal/store"
)

// FailoverRow is one domain that participates in replication, together with
// the actions its CURRENT state permits.
//
// Which actions a row offers is the whole safety design of this page. A
// console that offers every action on every row is one that invites the
// wrong one during an incident, when the person using it is under pressure
// and reading quickly. So eligibility is decided here, from observed state,
// and the template renders only what came back true.
//
// None of it is a substitute for the checks further down: vmsync refuses a
// promotion that is not justified and the agent refuses an operation whose
// peer does not match local metadata. This layer exists so that the common
// case never reaches those refusals in the first place.
type FailoverRow struct {
	// AgentID is the agent that would carry out an action on THIS domain.
	// Empty when no enrolled agent reports for its host, which makes every
	// action impossible rather than merely inadvisable.
	AgentID    string
	Hostname   string
	VM         string
	Role       string
	Active     bool
	AgentStale bool

	// PeerRef is the other end of the pair as THIS domain's own metadata
	// records it -- replica_source on a replica, the matching entry of
	// replica_targets on a source.
	PeerRef  string
	PeerHost string
	PeerVM   string
	// PeerSeen and the fields under it come from the PEER's own agent
	// report, not from this domain's metadata. That distinction is what
	// makes an inversion offer meaningful: a source's metadata cannot know
	// its target has been promoted, and the control plane is the only thing
	// that hears from both hosts.
	PeerSeen   bool
	PeerRole   string
	PeerActive bool

	// IsReplica and IsSource are what this domain is in the pair, which is
	// not the same question as its replication_role: a domain can carry no
	// role at all and still plainly be somebody's replica.
	IsReplica bool
	IsSource  bool

	// AllocatedBytes is what this domain's disks actually occupy, and
	// FreeBytes what remains on the storage under them. Shown because an
	// inversion's -replaced-disk-action=rename keeps the old copy, which
	// needs room for both at once -- the question being answered here is
	// "can I afford to keep it", and the honest place to answer it is
	// beside the button that spends the space.
	AllocatedBytes int64
	FreeBytes      int64
	FreeKnown      bool

	PromotedAtUnix int64
	PromotedBy     string
	PromotionMode  string
	LastSyncUnix   int64

	// ArmedFence is the fence THIS domain's promotion armed, so a promoted
	// row can say whether it authorised stopping its old source. A promoted
	// domain with none of this was a drill, or was promoted from a shell
	// without asking.
	ArmedFenceSource  string
	ArmedFenceAtUnix  int64
	ArmedFenceArmedBy string

	// Fenced is what this domain's own agent did about a fence naming it.
	// Nil for the overwhelming majority of domains, which have never been
	// fenced at all.
	Fenced *store.ReportFenced

	// RestorePoints is what this replica can be rolled back to, newest
	// first, exactly as its own agent last reported them.
	//
	// Straight from the report rather than derived: these are directories on
	// that host's filesystem, and this UI has no other way to know one
	// exists. That also means the list is as old as the report -- retention
	// prunes on every sync, so a tag shown here can be gone by the time a
	// button is pressed. Issuing one re-checks it against the latest report,
	// and the agent re-checks it against the filesystem.
	RestorePoints []store.ReportRestorePoint

	// The restore record, if this replica's disks were rolled back. Survives
	// a promotion, and is then the only thing that explains why the copy is
	// as old as it is.
	RestoredFrom   string
	RestoredAtUnix int64
	RestoredBy     string

	rank int
}

// WasFenced separates a domain a fence stopped from one somebody paused.
//
// The role now says so directly. It did not used to: both ended up `paused`
// with nothing in libvirt telling them apart, so this had to infer it from the
// agent's own fence ledger -- which only answers for a domain whose agent this
// UI can still reach, and whose entry has not aged out of a bounded file.
//
// The ledger is kept as a fallback rather than dropped, because a domain
// fenced by a vmsync old enough to have written `paused` still reads that way.
// It is also still the only thing that can say a fence FAILED (see
// FenceFailed): the role records that a fence happened, not whether it
// managed to stop the guest.
func (r FailoverRow) WasFenced() bool {
	return r.Role == store.RoleFenced || r.Fenced.Succeeded()
}

// FenceFailed is the alarm: a fence was attempted against this domain, did
// not stop it, and it is STILL RUNNING.
//
// Worth its own state because it is invisible everywhere else. The attempt
// leaves no mark in libvirt, the fence is latched so nothing retries it, and
// the domain simply keeps running beside a promoted copy -- looking exactly
// like a failover nobody has resolved yet, rather than one that was resolved
// and did not take.
//
// Requiring Active is what stops it becoming permanent noise. The ledger
// entry is latched forever by design, so a fence that failed in March and
// was then handled by somebody shutting the VM down would otherwise still be
// shouting in December, about a situation that no longer exists.
func (r FailoverRow) FenceFailed() bool {
	return r.Active && r.Fenced != nil && r.Fenced.State != store.OpStateDone
}

// FenceState is the single word for this row's fencing, or "" when it has
// never been fenced.
func (r FailoverRow) FenceState() string {
	switch {
	case r.WasFenced():
		return "fenced"
	case r.FenceFailed():
		return "fence failed"
	default:
		return ""
	}
}

// FencedAt renders when the fence acted, for the explanation.
func (r FailoverRow) FencedAt() string {
	if r.Fenced == nil || r.Fenced.AtUnix == 0 {
		return ""
	}
	return time.Unix(r.Fenced.AtUnix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// ArmedFenceAt renders when this domain's own promotion armed a fence.
func (r FailoverRow) ArmedFenceAt() string {
	if r.ArmedFenceAtUnix == 0 {
		return ""
	}
	return time.Unix(r.ArmedFenceAtUnix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// CanPromote reports whether this domain can be made to serve live.
//
// It must actually be somebody's replica: promoting a domain that is not one
// has no meaning, and vmsync would refuse it anyway. A source is excluded
// because promoting one would be a request to overwrite the original with
// its own copy. A promoted domain is excluded only while it is RUNNING --
// see the case below, which is what lets a half-finished failover be
// finished from here.
func (r FailoverRow) CanPromote() bool {
	if r.AgentID == "" || !r.IsReplica {
		return false
	}
	switch r.Role {
	case store.RoleSource:
		return false
	case store.RolePromoted:
		// Except when it was promoted and never started. That is a failover
		// stopped half way -- vmsync writes the promotion record BEFORE
		// booting the domain, so a crash, a refused start or an interrupted
		// operation all leave exactly this -- and vmsync's own -promote
		// handles it: it leaves the original promotion record alone and
		// starts the domain. Excluding it would leave the console able to
		// begin a failover and unable to finish one.
		return !r.Active
	}
	return true
}

// PromoteFinishesAStalledFailover distinguishes the case above, so the
// control can say what it will actually do. Offering a button labelled
// "Promote" on a domain that is already promoted would read as a mistake.
func (r FailoverRow) PromoteFinishesAStalledFailover() bool {
	return r.Role == store.RolePromoted && !r.Active
}

// CanShutdown reports whether a clean guest shutdown is offerable.
//
// Running is the only requirement. This is the source half of a planned
// failover -- stop the source, sync once more, then promote, which is the
// sequence that loses nothing -- and it is also how an operator undoes a
// promotion by hand.
func (r FailoverRow) CanShutdown() bool { return r.AgentID != "" && r.Active }

// CanInvert reports whether this pair's direction can be reversed.
//
// Offered on the OLD SOURCE's row, because that is the host -invert runs on:
// it already holds the SSH path to the other side, since that is the
// direction syncs run. The condition that cannot come from this domain's own
// metadata is the important one -- its peer must actually be promoted, which
// only something hearing from both hosts can know.
//
// PeerSeen carries a second requirement that is easy to read past. The index
// it comes from holds only live, non-revoked agents that have actually
// reported, so a peer being seen at all is what guarantees an enrolled agent
// exists for its host. That matters beyond tidiness: the schedule has to
// follow the inversion to the new source, and the store treats a missing
// agent there as a hard error while processing the very report carrying the
// outcome -- an inversion that succeeded on both hypervisors and then jammed
// its own result.
func (r FailoverRow) CanInvert() bool {
	return r.AgentID != "" && r.IsSource && r.PeerSeen && r.PeerRole == store.RolePromoted
}

// CanSetRole reports whether the role can be changed by hand.
//
// The escape hatch, and the way back from every state this page can reach:
// clearing `paused` after a fence, or putting a promoted domain back to
// `target` when the promotion turned out to be unwanted.
func (r FailoverRow) CanSetRole() bool { return r.AgentID != "" }

// CanRestore reports whether this domain can be rolled back to one of its
// restore points.
//
// Three requirements, and the middle one is the interesting one:
//
//   - It must have restore points. A domain with none is not a candidate,
//     and offering a control that opens onto an empty list is worse than
//     offering nothing.
//   - It must not be RUNNING. A restore replaces the disks underneath the
//     guest; vmsync refuses it, and so should the page.
//   - It must not be a source or promoted. Both hold live data that a
//     restore would overwrite with an old replica's, and vmsync's own gate
//     refuses both -- this mirrors it so the refusal arrives before the
//     click rather than after.
//
// `paused` is deliberately allowed, and it is the common case: a restore
// leaves the replica paused, so a first choice that turns out to be wrong can
// be followed by a second.
func (r FailoverRow) CanRestore() bool {
	if r.AgentID == "" || r.Active || len(r.RestorePoints) == 0 {
		return false
	}
	switch r.Role {
	case store.RoleSource, store.RolePromoted:
		return false
	}
	return true
}

// CanReinit reports whether a one-shot full resync can be asked for.
//
// Offered on the SOURCE's row, because that is the host it runs on -- a
// reinit is a sync, and it needs the pair's transport settings, which live on
// the schedule the source's agent holds.
//
// This is the second half of going back to replicating after a restore. The
// first is setting the replica's role back to `target`; without that this
// would rebuild a replica the far end still refuses to accept.
func (r FailoverRow) CanReinit() bool {
	return r.AgentID != "" && r.IsSource
}

// CanForceClean reports whether the destructive variant of a full resync can
// be asked for.
//
// Same predicate as CanReinit and offered from the same row for the same
// reason: it is a sync. Kept as its own method rather than reusing CanReinit
// in the template because the two answer different questions, and the day one
// of them grows a condition the other should not inherit, the template should
// not have to be rewritten to find out.
func (r FailoverRow) CanForceClean() bool {
	return r.CanReinit()
}

// SplitBrain is this row's own half of the condition the dashboard reports:
// this domain is running, and a promoted copy of it is running elsewhere.
func (r FailoverRow) SplitBrain() bool {
	return r.Active && r.PeerSeen && r.PeerRole == store.RolePromoted && r.PeerActive
}

// NeedsDecision marks the rows this page exists for, which is what puts them
// at the top rather than leaving them to be found by scrolling.
func (r FailoverRow) NeedsDecision() bool {
	return r.SplitBrain() || r.FenceFailed() || r.Role == store.RolePromoted ||
		r.Role == store.RolePaused || r.PeerPromoted()
}

// PeerPromoted is a pair that has been failed over and not yet resolved,
// seen from the OLD SOURCE's side.
//
// Worth ranking up even when it is not a split brain: this row is the one
// that can invert, and until somebody does, the pair is replicating in a
// direction that no longer matches which copy is authoritative.
func (r FailoverRow) PeerPromoted() bool {
	return r.PeerSeen && r.PeerRole == store.RolePromoted
}

// KeepingOldDisksFits reports whether an inversion could keep the displaced
// copy rather than deleting it.
//
// False when the figures are known and the space is not there. Unknown
// storage returns true: this decides whether to show a warning, and warning
// on the strength of a number nobody has would train people to ignore it.
func (r FailoverRow) KeepingOldDisksFits() bool {
	if !r.FreeKnown || r.AllocatedBytes == 0 {
		return true
	}
	return r.FreeBytes >= r.AllocatedBytes
}

// StorageKnown reports whether there is anything worth showing in the
// storage column. Rendering "0 B" and "unknown free" for a domain nobody
// measured is worse than a dash: it looks like a measurement of zero.
func (r FailoverRow) StorageKnown() bool { return r.AllocatedBytes > 0 || r.FreeKnown }

// Allocated and Free render the storage figures for display.
func (r FailoverRow) Allocated() string { return humanBytes(r.AllocatedBytes) }
func (r FailoverRow) Free() string {
	if !r.FreeKnown {
		return "unknown"
	}
	return humanBytes(r.FreeBytes)
}

// PromotedAt renders when the promotion happened.
func (r FailoverRow) PromotedAt() string {
	if r.PromotedAtUnix == 0 {
		return ""
	}
	return time.Unix(r.PromotedAtUnix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// WasRestored reports whether this domain's disks were rolled back to a
// restore point rather than being what the last sync copied.
func (r FailoverRow) WasRestored() bool { return r.RestoredFrom != "" }

// RestoredAt renders when the rollback was performed -- not when the copy it
// used was taken. Those are different instants and the gap between them is
// usually the interesting part of an incident timeline.
func (r FailoverRow) RestoredAt() string {
	if r.RestoredAtUnix == 0 {
		return ""
	}
	return time.Unix(r.RestoredAtUnix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// ContentsAsOf is how old this replica's DATA is, as distinct from how long
// ago anything happened to it.
//
// The one number a promotion decision turns on, and it was previously visible
// nowhere. inventory.Assess deliberately reports no age for a promoted or
// paused domain -- correctly, because a growing last-sync age is expected
// there and flagging it as staleness would bury the real signal -- so the
// staleness column shows a dash for exactly the rows an operator is staring
// at during a failover. This answers the other question from the same data.
func (r FailoverRow) ContentsAsOf() string {
	if r.LastSyncUnix == 0 {
		return ""
	}
	return time.Unix(r.LastSyncUnix, 0).UTC().Format("2006-01-02 15:04 UTC") +
		" (" + humanAge(time.Since(time.Unix(r.LastSyncUnix, 0))) + " old)"
}

// RestorePointView is one restore point as the page shows it.
//
// A view type rather than rendering store.ReportRestorePoint directly,
// because the two things an operator has to weigh -- how far back this would
// take them, and whether anyone ever checked this copy -- are both derived
// rather than stored.
type RestorePointView struct {
	Tag        string
	TakenAt    string
	Age        string
	Checkpoint string
	Disks      int
	Verify     string
	// Caveat spells out what the verify state does and does not promise, in
	// words rather than a term of art.
	Caveat     string
	Incomplete bool
}

// RestorePointViews renders this row's restore points for the page.
func (r FailoverRow) RestorePointViews() []RestorePointView {
	now := time.Now()
	out := make([]RestorePointView, 0, len(r.RestorePoints))
	for _, rp := range r.RestorePoints {
		// The instant the CONTENTS correspond to, not when the copy was
		// made. A checkpoint is taken before any data moves, so everything
		// written from then on belongs to the next one -- measuring from the
		// end of the copy understates how far back a restore goes by the
		// whole duration of that sync, which over a WAN is hours.
		at := rp.CheckpointAtUnix
		if at <= 0 {
			at = rp.TakenAtUnix
		}
		v := RestorePointView{
			Tag:        rp.Tag,
			TakenAt:    time.Unix(at, 0).UTC().Format("2006-01-02 15:04 UTC"),
			Age:        humanAge(now.Sub(time.Unix(at, 0))) + " ago",
			Checkpoint: rp.Checkpoint,
			Disks:      len(rp.Disks),
			Verify:     rp.Verify,
			Incomplete: rp.Incomplete,
		}
		switch {
		case rp.Incomplete:
			v.Caveat = "its record could not be read, so nothing is known about it beyond the fact that it exists"
		case rp.Verify == "passed":
			v.Caveat = "compared against the source at the time, and matched"
		case rp.Verify == "failed":
			v.Caveat = "compared against the source and MISMATCHED — this copy is known bad"
		default:
			v.Caveat = "never compared against the source; copies are taken before verification runs, so this is the usual state rather than a fault"
		}
		out = append(out, v)
	}
	return out
}

// OperationView is one issued operation, rendered.
type OperationView struct {
	store.OperationRecord
	// Host is the hypervisor the operation runs on, resolved from the
	// record's agent ID. A VM name alone does not say which end of a pair
	// is about to change, and during an incident that is the first thing
	// an operator needs.
	Host string
	// State is the single word describing where this stands, collapsing the
	// record's several independent fields into the one thing an operator
	// wants: pending, cancelled, expired, or whatever the agent reported.
	State string
	// Terminal is whether this is finished, one way or another.
	Terminal bool
	Created  string
	Age      string
	// Detail is the failure reason or the agent's log tail, whichever says
	// more. Empty on a clean success.
	Detail string
}

// StateClass colours an operation's state.
//
// Its own mapping rather than statusClass's, because the two vocabularies
// mean different things: a replication status describes whether a VM is
// protected, while this describes what became of one instruction. Sharing
// the function would render every one of these grey, which is the colour
// for "nothing to say" -- and a failed failover has a great deal to say.
func (o OperationView) StateClass() string {
	switch o.State {
	case store.OpStateDone:
		return "s-ok"
	case store.OpStateFailed:
		return "s-crit"
	case store.OpStateRefused, store.OpStateExpired, store.OpStateUnknown:
		// Not failures of the hypervisor, but not successes either: refused
		// and expired are the agent declining to act, and unknown is an
		// operation whose outcome nobody established.
		return "s-warn"
	case "pending":
		return "s-admin"
	default:
		return "s-none"
	}
}

// FailoverView is the whole page.
type FailoverView struct {
	Rows []FailoverRow
	// Operations is what the page renders: every operation still awaiting an
	// agent, plus the most recent finished ones.
	//
	// Capped because nothing prunes the store -- every failover the estate
	// has ever performed is kept, and an unbounded table would get slower and
	// less readable every year, which is the opposite of what a page read
	// during an incident needs. Pending ones are NEVER dropped by the cap:
	// they are the actionable rows, they carry the only Cancel button, and
	// one hidden behind a page limit is one blocking its VM invisibly.
	Operations []OperationView
	// OlderHidden is how many finished operations the cap left out, so the
	// list says it is partial rather than looking complete.
	OlderHidden int
	// Pending counts the operations still awaiting an agent. Shown because
	// a pending operation blocks any other operation on the same VM, and an
	// operator who does not know that reads the refusal as a bug.
	Pending int
	// SplitBrainCount is how many rows have both copies running. Counted
	// here rather than in the template: a page that has to loop and tally to
	// render its own headline is one where the headline can silently
	// disagree with the rows beneath it.
	SplitBrainCount int
	// FenceFailedCount is how many domains a fence tried and failed to stop
	// and which are still running. Counted separately from SplitBrainCount
	// because the two call for different responses: one may simply be a
	// failover nobody has finished, while this one has already been acted on
	// unsuccessfully and will not be retried by anything.
	FenceFailedCount int
	// UrgentCount is how many DOMAINS are in trouble right now, which is not
	// the sum of the two counts above: a VM running in two places whose
	// fence also failed is one problem described twice, and two chips
	// summing to 2 would have an operator looking for a second VM.
	UrgentCount int
	// TTLMinutes is how long an issued operation stays executable, so the
	// page can say so rather than leaving an operator to discover it.
	TTLMinutes int
	// NoAgents is true when nothing is enrolled at all, which turns an empty
	// table from a puzzle into a sentence.
	NoAgents bool
}

// BuildFailoverView assembles the page from every agent's latest report.
func BuildFailoverView(
	agents []store.Agent,
	reports map[string]store.Report,
	ops []store.OperationRecord,
	now time.Time,
) FailoverView {
	v := FailoverView{TTLMinutes: int(store.OperationTTL.Minutes())}

	// Index every reported domain by "host:vm" first. Cross-referencing is
	// the point of this page -- whether a source's target has been promoted
	// is not in the source's own metadata -- and it needs the whole fleet
	// in hand before any row can be finished.
	// Indexed by "host:vm" so a row can ask about its peer, which lives in a
	// different host's report entirely. Only live, non-revoked agents that
	// have actually reported get in, which is what lets PeerSeen stand for
	// "an enrolled agent exists for that host".
	byRef := map[string]store.ReportDomain{}
	live := 0
	for _, a := range agents {
		if a.Revoked {
			continue
		}
		live++
		rep, ok := reports[a.ID]
		if !ok {
			continue
		}
		host := rep.Hostname
		if host == "" {
			host = a.Hostname
		}
		for _, dom := range rep.Domains {
			byRef[refKey(host, dom.Name)] = dom
		}
	}
	v.NoAgents = live == 0

	// hostByAgent resolves operation targets to hypervisor names for the
	// operations table below. Report hostname first (what the host calls
	// itself now), enrolled name as fallback. Revoked agents stay in:
	// the operation ran, or is still waiting, on that host regardless.
	hostByAgent := map[string]string{}
	for _, a := range agents {
		name := a.Hostname
		if rep, ok := reports[a.ID]; ok && rep.Hostname != "" {
			name = rep.Hostname
		}
		hostByAgent[a.ID] = name
	}

	for _, a := range agents {
		if a.Revoked {
			continue
		}
		rep, ok := reports[a.ID]
		if !ok {
			continue
		}
		host := rep.Hostname
		if host == "" {
			host = a.Hostname
		}
		stale := a.LastSeenAt == 0 || now.Sub(time.Unix(a.LastSeenAt, 0)) > staleAfter
		free := freeByDirectory(rep.Filesystems)

		for _, dom := range rep.Domains {
			if dom.ReplicaSource == "" && len(dom.ReplicaTargets) == 0 {
				// Not part of any pair. The dashboard already names these as
				// unprotected; there is nothing to fail over here.
				continue
			}
			row := FailoverRow{
				AgentID:        a.ID,
				Hostname:       host,
				VM:             dom.Name,
				Role:           dom.Role,
				Active:         dom.Active,
				AgentStale:     stale,
				IsReplica:      dom.ReplicaSource != "",
				IsSource:       len(dom.ReplicaTargets) > 0,
				AllocatedBytes: dom.AllocatedBytes(),
				PromotedAtUnix: dom.PromotedAtUnix,
				PromotedBy:     dom.PromotedBy,
				PromotionMode:  dom.PromotionMode,
				LastSyncUnix:   dom.LastSyncUnix,

				ArmedFenceSource:  dom.FenceSource,
				ArmedFenceAtUnix:  dom.FenceArmedAtUnix,
				ArmedFenceArmedBy: dom.FenceArmedBy,
				Fenced:            dom.Fenced,
				RestorePoints:     dom.RestorePoints,
				RestoredFrom:      dom.RestoredFrom,
				RestoredAtUnix:    dom.RestoredAtUnix,
				RestoredBy:        dom.RestoredBy,
			}
			row.FreeBytes, row.FreeKnown = freeFor(dom.Disks, free)

			// Which peer this row is about. A replica has exactly one
			// source, so that is unambiguous. A source may fan out, and the
			// one worth offering actions against is any target that has been
			// promoted -- that being the situation this page is for.
			switch {
			case dom.ReplicaSource != "":
				row.PeerRef = dom.ReplicaSource
			case len(dom.ReplicaTargets) > 0:
				row.PeerRef = pickPromotedTarget(dom.ReplicaTargets, byRef)
			}
			row.PeerHost, row.PeerVM = splitRef(row.PeerRef)
			if peer, ok := byRef[refKey(row.PeerHost, row.PeerVM)]; ok {
				row.PeerSeen = true
				row.PeerRole = peer.Role
				row.PeerActive = peer.Active
			}

			row.rank = rankRow(row)
			if row.SplitBrain() {
				v.SplitBrainCount++
			}
			if row.FenceFailed() {
				v.FenceFailedCount++
			}
			if row.SplitBrain() || row.FenceFailed() {
				v.UrgentCount++
			}
			v.Rows = append(v.Rows, row)
		}
	}

	// What needs a decision first, then by host and VM. Same reasoning as
	// the schedule page: the rows that brought somebody here should not have
	// to be looked for.
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].rank != v.Rows[j].rank {
			return v.Rows[i].rank < v.Rows[j].rank
		}
		if v.Rows[i].Hostname != v.Rows[j].Hostname {
			return v.Rows[i].Hostname < v.Rows[j].Hostname
		}
		return v.Rows[i].VM < v.Rows[j].VM
	})

	terminalShown := 0
	for _, rec := range ops {
		ov := OperationView{
			OperationRecord: rec,
			Created:         time.Unix(rec.CreatedAtUnix, 0).UTC().Format("2006-01-02 15:04 UTC"),
			Age:             humanAge(now.Sub(time.Unix(rec.CreatedAtUnix, 0))),
		}
		// An operation against an agent ID nobody has heard of should not
		// happen -- agents are revoked, never deleted -- but a blank cell
		// would be worse than the raw ID if it ever did.
		if h, ok := hostByAgent[rec.AgentID]; ok && h != "" {
			ov.Host = h
		} else {
			ov.Host = rec.AgentID
		}
		switch {
		case rec.CancelledAtUnix != 0:
			ov.State, ov.Terminal = "cancelled", true
			ov.Detail = "cancelled by " + rec.CancelledBy
		case rec.Result != nil:
			ov.State, ov.Terminal = rec.Result.State, true
			ov.Detail = rec.Result.Error
			if ov.Detail == "" {
				ov.Detail = lastLine(rec.Result.LogTail)
			}
		case rec.Expired(now):
			// Still published, deliberately: the agent has to see it, refuse
			// it and say so, which is what closes the audit entry with a
			// reason instead of leaving it pending forever.
			ov.State = "expired"
			ov.Detail = "past its deadline; the agent will refuse it and report that"
			v.Pending++
		default:
			ov.State = "pending"
			v.Pending++
		}

		if ov.Terminal {
			// ops arrives newest-first, so counting past the cap here keeps
			// the most recent ones and drops the oldest.
			terminalShown++
			if terminalShown > operationsShown {
				v.OlderHidden++
				continue
			}
		}
		v.Operations = append(v.Operations, ov)
	}
	return v
}

// operationsShown bounds the finished operations the page renders. Matches
// the agent's own resultsKept, for no deeper reason than that two different
// numbers for "recent enough to still care about" would be arbitrary twice.
const operationsShown = 50

// rankRow orders by how much attention a row wants.
func rankRow(r FailoverRow) int {
	switch {
	case r.SplitBrain(), r.FenceFailed():
		// Two live copies of one VM, diverging from the moment the second
		// started. Nothing else on this page outranks it.
		//
		// A failed fence sits here too, and is arguably the worse of the
		// two: something already tried to resolve this and could not, and
		// nothing will try again. The inferred split brain at least might be
		// a failover nobody has got to yet.
		return 0
	case r.Role == store.RolePromoted:
		return 1
	case r.PeerPromoted():
		// The old source of an unresolved failover. This is the row that
		// carries the Invert button, so burying it below the quiet rows
		// would hide the resolution rather than the problem.
		return 2
	case r.Role == store.RolePaused:
		// Includes every domain a fence stopped. Ranked above the quiet rows
		// because a paused replica is not being protected by anything.
		return 3
	default:
		return 4
	}
}

// pickPromotedTarget chooses which of a source's targets a row is about.
//
// A promoted one if there is one, because that is the pair in an unresolved
// state and the only one an action here applies to. Otherwise the first, so
// the ordinary single-target case still shows its peer.
func pickPromotedTarget(targets []string, byRef map[string]store.ReportDomain) string {
	for _, t := range targets {
		h, vm := splitRef(t)
		if peer, ok := byRef[refKey(h, vm)]; ok && peer.Role == store.RolePromoted {
			return t
		}
	}
	if len(targets) > 0 {
		return targets[0]
	}
	return ""
}

// refKey normalises a "host:vm" pair for lookup. Case-insensitive on the
// host and exact on the VM, matching how vmsync itself compares the two
// halves everywhere else -- hostnames are case-insensitive, libvirt domain
// names are not.
func refKey(host, vm string) string {
	return strings.ToLower(strings.TrimSpace(host)) + ":" + strings.TrimSpace(vm)
}

// freeByDirectory indexes reported filesystems by the directory they back.
func freeByDirectory(fss []store.ReportFilesystem) map[string]int64 {
	out := make(map[string]int64, len(fss))
	for _, fs := range fss {
		out[fs.Path] = fs.FreeBytes
	}
	return out
}

// freeFor finds the space behind a domain's disks.
//
// The smallest free figure across every filesystem the disks sit on, because
// a domain spanning two of them can only keep an extra copy if BOTH have
// room -- reporting the roomier one would answer a question nobody asked.
func freeFor(disks []store.ReportDisk, free map[string]int64) (int64, bool) {
	var smallest int64
	found := false
	for _, d := range disks {
		dir := path.Dir(d.Path)
		v, ok := free[dir]
		if !ok {
			continue
		}
		if !found || v < smallest {
			smallest, found = v, true
		}
	}
	return smallest, found
}

// humanBytes renders a size the way an operator reads one.
//
// Binary units, because that is what qemu-img, df and libvirt all report,
// and a console that quietly disagreed with them by 7% would be worse than
// one that showed raw bytes.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
