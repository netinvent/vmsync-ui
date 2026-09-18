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
	"sort"
	"strings"
	"time"

	"vmsync-ui/internal/store"
)

// Pair is one replication relationship, assembled from what the agents on
// both sides reported.
//
// The row is built from the TARGET's report, not the source's. vmsync writes
// last_sync, last_checkpoint and failure_count onto the target domain, so the
// target is where a pair's freshness actually lives -- a source's own
// metadata records where it replicates to, never when.
type Pair struct {
	SourceHost string
	SourceVM   string
	TargetHost string
	TargetVM   string

	Status     string
	Reasons    []string
	AgeSeconds int64

	LastCheckpoint string
	FailureCount   int
	Role           string
	TargetActive   bool

	// SourceSeen is false when no agent reported a source domain matching
	// this target's replica_source. That is worth showing: it means either
	// the source host has no agent, or its agent is down, and in both cases
	// this pair is only half-observed.
	SourceSeen bool
	// SourceActive is the source domain's runtime state as its own agent
	// reported it, meaningful only when SourceSeen.
	SourceActive bool
	// SourceReportAgeSeconds is how old the report SourceActive came from
	// is, or -1 when the source was never reported at all.
	//
	// This exists because a stored report is not evidence of anything
	// current. Reports are replaced in place and never aged out, so a host
	// that died while its VM was running says Active:true for as long as
	// the record survives -- which is forever.
	SourceReportAgeSeconds int64
}

// SourceFresh reports whether the source's runtime state is current enough
// to reason about, as opposed to a frozen last-known value from a host that
// may have been gone for a week.
func (p Pair) SourceFresh() bool {
	return p.SourceSeen && p.SourceReportAgeSeconds >= 0 &&
		time.Duration(p.SourceReportAgeSeconds)*time.Second <= staleAfter
}

// SplitBrain reports both ends of a pair running at once: the target has
// been promoted to serve live, and the original source is running too.
//
// vmsync cannot prevent this. A forced promotion during a network partition
// promotes a replica while the original may still be up and serving, and
// nothing in a replication tool with no power control can fence the other
// side. What a control plane CAN do is notice, because it is the only thing
// that hears from both hosts -- neither agent can see the other.
//
// So this is new visibility, not new risk: the condition already occurred,
// it was simply invisible until something looked at both reports together.
//
// It requires the source's report to be CURRENT, and that requirement is
// what keeps the alarm worth reading. The case this detector exists for --
// a forced promotion during a site outage -- is also the case where the
// source's last stored report says Active:true and will keep saying so
// forever, because the host that would correct it is gone. Without the
// freshness gate the banner fires on every correctly-executed failover of a
// genuinely dead primary, and an alarm that is indistinguishable from
// success is one operators learn to scroll past.
func (p Pair) SplitBrain() bool {
	return p.promotedAndSourceRunning() && p.SourceFresh()
}

// SplitBrainPossible is the honest middle state: the target was promoted
// and is running, and the last thing anyone heard about the source was that
// it was running too -- but nobody has heard from it recently enough to say
// whether that is still true.
//
// Reported separately and quietly. Merging it into SplitBrain would cry
// wolf; dropping it would claim a dead source as fact when all that is
// actually known is silence.
func (p Pair) SplitBrainPossible() bool {
	return p.promotedAndSourceRunning() && !p.SourceFresh()
}

func (p Pair) promotedAndSourceRunning() bool {
	return p.Role == store.RolePromoted && p.TargetActive && p.SourceSeen && p.SourceActive
}

// SourceLastSeen renders how long ago the source's runtime state was
// observed, for the uncertain case above.
func (p Pair) SourceLastSeen() string {
	if p.SourceReportAgeSeconds < 0 {
		return "never"
	}
	return humanAge(time.Duration(p.SourceReportAgeSeconds) * time.Second)
}

// PromotedIdle is a promoted target that is not running while its original
// source still is. Not split-brain, but worth surfacing on its own: it is
// either a failover that stopped half way, or a promotion someone performed
// and then thought better of.
func (p Pair) PromotedIdle() bool {
	return p.Role == store.RolePromoted && !p.TargetActive && p.SourceSeen && p.SourceActive
}

// AgentView is one enrolled agent's liveness, as opposed to the replication
// state it reported.
type AgentView struct {
	ID           string
	Hostname     string
	Version      string
	LastSeen     string
	LastSeenUnix int64
	Stale        bool
	Revoked      bool
	// Mode is what the agent reports itself as: "monitor" or "controlled",
	// and empty from one that has never said. Shown on this page because an
	// agent's liveness is exactly where "it is alive, and it is not running
	// anything I publish" needs to be visible -- a monitor agent is green by
	// every other measure here.
	Mode string
	// ReadOnly is Mode == monitor, precomputed so the template asks a
	// question rather than comparing strings.
	ReadOnly bool
	// ConfigAge is how long since the agent last confirmed its configuration
	// with this UI. An agent running from cache during a partition is
	// expected, not broken -- but an operator has to be able to see it.
	ConfigAge string
	Domains   int
}

// Dashboard is everything the availability page renders.
type Dashboard struct {
	Pairs []Pair
	// Unprotected lists domains no agent reported as participating in
	// replication. Deliberately separate and prominent: "nobody configured
	// replication for this" must never be mistaken for "this is protected".
	Unprotected []Unprotected
	// SplitBrain lists pairs with both ends running at once. Rendered above
	// everything else, because a green board with this buried in it is worse
	// than no board: two live copies of one VM diverge from the moment it
	// starts, and every minute it goes unnoticed is data that will have to
	// be thrown away by hand.
	SplitBrain []Pair
	// SplitBrainPossible lists the same shape where the source has simply
	// gone quiet, so nobody can say whether it is still running. Kept out of
	// the banner on purpose: this is the state a correct forced failover of
	// a genuinely dead primary leaves behind, and alarming on it would make
	// the alarm mean "a failover happened".
	SplitBrainPossible []Pair
	Agents             []AgentView
	Counts             map[string]int
	// MissingAgents names hosts referenced as a replica source or target by
	// some domain, but which no enrolled agent reports for. Those are the
	// blind spots in the picture.
	MissingAgents []string
	// MissingTargets names sources with a reference no report resolves,
	// shown verbatim. The replica was deleted, never created -- or the
	// reference itself is misspelled, or uses a short name where the agent
	// reports an FQDN. Matching stays exact (case-insensitive only) so a
	// naming problem reads as a naming problem instead of being resolved
	// away. A reference is skipped when the source already has a pair row
	// for a target under the same VM name: the pair proves the copy exists,
	// so the spelling difference is not a missing copy.
	MissingTargets []MissingTarget
	GeneratedAt   string
}

type Unprotected struct {
	Host   string
	VM     string
	Active bool
}

// MissingTarget is one source-to-target reference that resolves to
// nothing, shown exactly as written.
type MissingTarget struct {
	// SourceHost/SourceVM name the source, as its own agent reported it.
	SourceHost string
	SourceVM   string
	// TargetHost/TargetVM are the reference as written in the source's own
	// metadata -- the name to go looking for on the target host, not a
	// guess at what it is called now.
	TargetHost string
	TargetVM   string
	// PeerKnown is false when no agent reports under the target's exact
	// name at all: a typo, or a short name where the agent reports an
	// FQDN. The row is the way back to the spelling mistake.
	PeerKnown bool
	// PeerStale is true when the target host IS known but its report is
	// old. Absence from an old report is weak evidence, but hiding the row
	// would trade a qualified warning for silence.
	PeerStale bool
}

// staleAfter is how long without a report before an agent is called stale.
// Generous relative to the default 60s report interval, so a single missed
// report or a slow link does not raise a false alarm.
const staleAfter = 5 * time.Minute

// BuildDashboard correlates every agent's latest report into pairs.
func BuildDashboard(agents []store.Agent, reports map[string]store.Report, now time.Time) Dashboard {
	d := Dashboard{
		Counts:      map[string]int{},
		GeneratedAt: now.Format("2006-01-02 15:04:05 MST"),
	}

	// Index every reported domain by "host:vm" so a target's replica_source
	// can be resolved back to the domain the other agent reported.
	type located struct {
		host   string
		domain store.ReportDomain
		// reportedAt is when the report this came from was received. Carried
		// so a consumer can tell a current observation from a frozen one.
		reportedAt int64
	}
	byRef := map[string]located{}
	hostsWithAgents := map[string]bool{}
	// hostFresh marks the hosts at least one agent reported for recently,
	// so a missing target on a stale peer renders as unconfirmed rather
	// than gone.
	hostFresh := map[string]bool{}

	for _, a := range agents {
		rep, ok := reports[a.ID]
		if !ok {
			continue
		}
		host := rep.Hostname
		if host == "" {
			host = a.Hostname
		}
		key := strings.ToLower(host)
		hostsWithAgents[key] = true
		if a.LastSeenAt > 0 && now.Sub(time.Unix(a.LastSeenAt, 0)) <= staleAfter {
			hostFresh[key] = true
		}
		for _, dom := range rep.Domains {
			byRef[strings.ToLower(host+":"+dom.Name)] = located{host: host, domain: dom, reportedAt: rep.ReportedAtUnix}
		}
	}

	referencedHosts := map[string]bool{}
	// missingCandidates holds dangling source-to-target references until
	// the pairs are all built: a reference the pairs already cover is a
	// spelling difference, not a missing copy, and must not be reported.
	var missingCandidates []MissingTarget

	for _, a := range agents {
		rep, ok := reports[a.ID]
		if !ok {
			continue
		}
		host := rep.Hostname
		if host == "" {
			host = a.Hostname
		}

		for _, dom := range rep.Domains {
			switch {
		case dom.ReplicaSource != "":
			srcHost, srcVM := splitRef(dom.ReplicaSource)
			if srcHost != "" {
				referencedHosts[strings.ToLower(srcHost)] = true
			}
			src, seen := byRef[strings.ToLower(dom.ReplicaSource)]
				p := Pair{
					SourceHost:     srcHost,
					SourceVM:       srcVM,
					TargetHost:     host,
					TargetVM:       dom.Name,
					Status:         dom.Status,
					Reasons:        dom.Reasons,
					AgeSeconds:     dom.AgeSeconds,
					LastCheckpoint: dom.LastCheckpoint,
					FailureCount:   dom.FailureCount,
					Role:           dom.Role,
					TargetActive:   dom.Active,
					SourceSeen:     seen,
					SourceActive:   seen && src.domain.Active,
				}
				p.SourceReportAgeSeconds = -1
				if seen && src.reportedAt > 0 {
					p.SourceReportAgeSeconds = int64(now.Sub(time.Unix(src.reportedAt, 0)).Seconds())
				}
				d.Pairs = append(d.Pairs, p)
				d.Counts[dom.Status]++
				switch {
				case p.SplitBrain():
					d.SplitBrain = append(d.SplitBrain, p)
					d.Counts["split-brain"]++
				case p.SplitBrainPossible():
					d.SplitBrainPossible = append(d.SplitBrainPossible, p)
				}

		case len(dom.ReplicaTargets) > 0:
			// A source. Its pairs are rendered from the target side; all
			// that is recorded here is which hosts it points at, so a
			// target host with no agent shows up as a blind spot.
			for _, ref := range dom.ReplicaTargets {
				tgtHost, tgtVM := splitRef(ref)
				if tgtHost == "" {
					// A bare VM name with no host half. Nothing can resolve
					// it to a peer report, so there is no absence to report.
					continue
				}
			referencedHosts[strings.ToLower(tgtHost)] = true
			// The replica this source names is in no agent's report. Kept
			// as a candidate for now: whether it is reported below depends
			// on the pairs, which are still being built. Matching is
			// deliberately exact (case-insensitive only): resolving a
			// short name against an FQDN report would hide the
			// misconfiguration instead of showing it.
			if _, ok := byRef[strings.ToLower(tgtHost+":"+tgtVM)]; !ok {
				peer := strings.ToLower(tgtHost)
				known := hostsWithAgents[peer]
				missingCandidates = append(missingCandidates, MissingTarget{
					SourceHost: host,
					SourceVM:   dom.Name,
					TargetHost: tgtHost,
					TargetVM:   tgtVM,
					PeerKnown:  known,
					PeerStale:  known && !hostFresh[peer],
				})
			}
			}

			default:
				if dom.Status == "unreplicated" {
					d.Unprotected = append(d.Unprotected, Unprotected{Host: host, VM: dom.Name, Active: dom.Active})
					d.Counts["unreplicated"]++
				}
			}
		}
	}

	for host := range referencedHosts {
		if !hostsWithAgents[host] {
			d.MissingAgents = append(d.MissingAgents, host)
		}
	}
	sort.Strings(d.MissingAgents)

	// A dangling reference is only alarming when the source has no pair row
	// for a target under the same VM name: a pair proves the copy exists
	// and shows its freshness, so claiming "no copy" alongside it would
	// contradict the board. The usual cause is the two metadata directions
	// spelling the peer differently (short on the source side, FQDN on the
	// target side); both spellings stay on display in their rows' names.
	// Matching here is exact like everywhere else -- no hostname is
	// transformed to make the comparison succeed.
	covered := map[string]bool{}
	for _, p := range d.Pairs {
		covered[p.SourceHost+"\x00"+p.SourceVM+"\x00"+p.TargetVM] = true
	}
	for _, m := range missingCandidates {
		if covered[m.SourceHost+"\x00"+m.SourceVM+"\x00"+m.TargetVM] {
			continue
		}
		d.MissingTargets = append(d.MissingTargets, m)
	}
	if len(d.MissingTargets) > 0 {
		d.Counts["missing-target"] = len(d.MissingTargets)
	}

	// Worst first: an availability page is read to find what needs
	// attention, so burying a critical pair under a page of healthy ones
	// would defeat the point.
	sort.SliceStable(d.Pairs, func(i, j int) bool {
		// Split-brain outranks every status, including critical. A status is
		// a judgement about replication being behind; this is a judgement
		// about two machines writing to the same identity right now.
		if bi, bj := d.Pairs[i].SplitBrain(), d.Pairs[j].SplitBrain(); bi != bj {
			return bi
		}
		si, sj := statusRank(d.Pairs[i].Status), statusRank(d.Pairs[j].Status)
		if si != sj {
			return si > sj
		}
		if d.Pairs[i].TargetHost != d.Pairs[j].TargetHost {
			return d.Pairs[i].TargetHost < d.Pairs[j].TargetHost
		}
		return d.Pairs[i].TargetVM < d.Pairs[j].TargetVM
	})
	sort.Slice(d.Unprotected, func(i, j int) bool {
		if d.Unprotected[i].Host != d.Unprotected[j].Host {
			return d.Unprotected[i].Host < d.Unprotected[j].Host
		}
		return d.Unprotected[i].VM < d.Unprotected[j].VM
	})
	sort.Slice(d.MissingTargets, func(i, j int) bool {
		if d.MissingTargets[i].SourceHost != d.MissingTargets[j].SourceHost {
			return d.MissingTargets[i].SourceHost < d.MissingTargets[j].SourceHost
		}
		if d.MissingTargets[i].SourceVM != d.MissingTargets[j].SourceVM {
			return d.MissingTargets[i].SourceVM < d.MissingTargets[j].SourceVM
		}
		if d.MissingTargets[i].TargetHost != d.MissingTargets[j].TargetHost {
			return d.MissingTargets[i].TargetHost < d.MissingTargets[j].TargetHost
		}
		return d.MissingTargets[i].TargetVM < d.MissingTargets[j].TargetVM
	})

	for _, a := range agents {
		v := AgentView{
			ID:       a.ID,
			Hostname: a.Hostname,
			Version:  a.AgentVersion,
			Revoked:  a.Revoked,
			Mode:     a.Mode,
			ReadOnly: a.ReadOnly(),
		}
		if a.LastSeenAt > 0 {
			seen := time.Unix(a.LastSeenAt, 0)
			v.LastSeenUnix = a.LastSeenAt
			v.LastSeen = humanAge(now.Sub(seen))
			v.Stale = now.Sub(seen) > staleAfter
		} else {
			v.LastSeen = "never"
			v.Stale = true
		}
		if rep, ok := reports[a.ID]; ok {
			v.Domains = len(rep.Domains)
			switch {
			case rep.ConfigAgeSeconds < 0:
				v.ConfigAge = "never fetched"
			default:
				v.ConfigAge = humanAge(time.Duration(rep.ConfigAgeSeconds) * time.Second)
			}
		}
		d.Agents = append(d.Agents, v)
	}
	sort.Slice(d.Agents, func(i, j int) bool { return d.Agents[i].Hostname < d.Agents[j].Hostname })

	return d
}

func splitRef(ref string) (host, vm string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", ref
	}
	return ref[:i], ref[i+1:]
}

// statusRank orders statuses by how much attention they want. Mirrors
// pkg/inventory's own ordering in the agent.
func statusRank(s string) int {
	switch s {
	case "critical":
		return 5
	case "warning":
		return 4
	case store.RolePromoted:
		return 3
	case store.RolePaused:
		return 2
	case "ok":
		return 1
	default: // unreplicated and anything a newer agent invents
		return 0
	}
}

func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// AgeString renders a pair's replication lag, or a dash when there is none
// to report -- a never-synced target has no age, and 0 would read as "just
// synced", which is the opposite of the truth.
func (p Pair) AgeString() string {
	if p.AgeSeconds < 0 {
		return "—"
	}
	return humanAge(time.Duration(p.AgeSeconds) * time.Second)
}
