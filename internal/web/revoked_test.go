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
	"testing"

	"vmsync-ui/internal/store"
)

// A revoked agent's report is frozen, and a frozen report must not read as
// protection -- but it must not vanish either.
//
// Revoking keeps the agent record (so audit entries resolve an ID to a
// hostname) and keeps its last report, while the agent can no longer
// authenticate to replace it. Every LastSeenAt below is the test clock,
// because that is the dangerous case rather than a contrived one: an agent
// revoked a moment ago looks as fresh as any live one.
//
// The first draft of this fix simply skipped revoked agents, and these tests
// are mostly about why that was wrong. Cutting a host off stops this console
// hearing from it; it does not stop a VM running, a replica existing, or a
// rebuild sitting half-written.
func TestARevokedAgentIsMarkedNotDropped(t *testing.T) {
	// The control. Identical except for the revocation, so a change that
	// marked everything withdrawn would fail here rather than pass quietly.
	t.Run("a live pair is not marked withdrawn", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{
				{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
				{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
			},
			map[string]store.Report{
				"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
				}},
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
				}},
			}, now)

		if len(d.Pairs) != 1 {
			t.Fatalf("got %d pairs, want 1", len(d.Pairs))
		}
		if d.Pairs[0].Withdrawn() {
			t.Errorf("a live pair is marked withdrawn: %+v", d.Pairs[0])
		}
		if d.Pairs[0].WithdrawnEnd() != "" {
			t.Errorf("WithdrawnEnd = %q for a live pair, want empty", d.Pairs[0].WithdrawnEnd())
		}
	})

	t.Run("a revoked target keeps its row and is marked", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{
				{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
				{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()},
			},
			map[string]store.Report{
				"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
				}},
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
				}},
			}, now)

		if len(d.Pairs) != 1 {
			t.Fatalf("got %d pairs, want 1 -- dropping the row takes the replica off the board, and it is still on disk", len(d.Pairs))
		}
		p := d.Pairs[0]
		if !p.TargetWithdrawn || !p.Withdrawn() {
			t.Errorf("the row is not marked withdrawn, so a frozen report reads as current protection: %+v", p)
		}
		if p.WithdrawnEnd() != "hyper02p" {
			t.Errorf("WithdrawnEnd = %q, want hyper02p -- the warning has to name the host nobody hears from", p.WithdrawnEnd())
		}
		// The host was withdrawn by this console, not misspelled by anyone.
		if len(d.MissingAgents) != 0 {
			t.Errorf("MissingAgents = %v, want none: the host is known, it was revoked", d.MissingAgents)
		}
		if len(d.MissingTargets) != 0 {
			t.Errorf("MissingTargets = %+v, want none: the copy exists and is on the board", d.MissingTargets)
		}
	})

	t.Run("a revoked source keeps its row and is marked", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{
				{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()},
				{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
			},
			map[string]store.Report{
				"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok", Active: true},
				}},
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
				}},
			}, now)

		if len(d.Pairs) != 1 {
			t.Fatalf("got %d pairs, want 1", len(d.Pairs))
		}
		if !d.Pairs[0].SourceWithdrawn {
			t.Error("the source end is not marked withdrawn")
		}
		if !d.Pairs[0].SourceSeen {
			t.Error("SourceSeen went false for a source that WAS reported, just not recently -- that is what SourceWithdrawn is for, and conflating the two loses the split-brain predicate")
		}
	})

	// THE REGRESSION THE FIRST DRAFT CAUSED. Split-brain is the most serious
	// thing this page shows, and skipping revoked agents silenced it in both
	// directions: with the promoted end revoked the row vanished entirely,
	// and with the source revoked SourceSeen went false, which is half of
	// promotedAndSourceRunning.
	for _, tc := range []struct {
		name            string
		revokeSrc       bool
		revokeTgt       bool
		wantWithdrawnBy string
	}{
		{name: "nobody revoked", wantWithdrawnBy: ""},
		{name: "the promoted copy's agent is revoked", revokeTgt: true, wantWithdrawnBy: "hyper02p"},
		{name: "the old primary's agent is revoked", revokeSrc: true, wantWithdrawnBy: "hyper01p"},
	} {
		t.Run("split-brain survives when "+tc.name, func(t *testing.T) {
			d := BuildDashboard(
				[]store.Agent{
					{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix(), Revoked: tc.revokeSrc},
					{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix(), Revoked: tc.revokeTgt},
				},
				map[string]store.Report{
					"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 10, Domains: []store.ReportDomain{
						{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok", Active: true},
					}},
					"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 10, Domains: []store.ReportDomain{
						{Name: "web01", ReplicaSource: "hyper01p:web01", Role: store.RolePromoted, Active: true, Status: "ok"},
					}},
				}, now)

			if len(d.SplitBrain) != 1 {
				t.Fatalf("SplitBrain = %d rows, want 1 -- two copies of web01 are running and the banner is the only thing that says so. Revoking an agent stops this console hearing from a host; it does not stop a VM", len(d.SplitBrain))
			}
			if d.Counts["split-brain"] != 1 {
				t.Errorf("Counts[split-brain] = %d, want 1", d.Counts["split-brain"])
			}
			if got := d.SplitBrain[0].WithdrawnEnd(); got != tc.wantWithdrawnBy {
				t.Errorf("WithdrawnEnd = %q, want %q -- the banner has to say which end is no longer observed", got, tc.wantWithdrawnBy)
			}
		})
	}

	// hostFresh is withheld from a revoked agent, and this is the only place
	// it is observable: a dangling reference to a KNOWN host renders as
	// unconfirmed rather than gone when that host's report is not current.
	t.Run("a revoked host is never fresh", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{
				{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
				{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()},
			},
			map[string]store.Report{
				"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					// Names a replica the revoked host never reported, so this
					// is a genuine dangling reference rather than a pair.
					{Name: "db01", ReplicaTargets: []string{"hyper02p:db01"}, Status: "ok"},
				}},
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok"},
				}},
			}, now)

		if len(d.MissingTargets) != 1 {
			t.Fatalf("MissingTargets = %+v, want 1", d.MissingTargets)
		}
		m := d.MissingTargets[0]
		if !m.PeerKnown {
			t.Error("PeerKnown is false for a host this console revoked, so the page blames a typo for a hole an operator made deliberately")
		}
		if !m.PeerStale {
			t.Error("PeerStale is false for a revoked host: its LastSeenAt is recent at the instant of revocation, so without withholding freshness the reference reads as confirmed-gone when nobody can confirm anything about that host again")
		}
	})

	// Everything else the first draft dropped along with the pairs.
	t.Run("a revoked host's other facts survive", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{
				{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()},
			},
			map[string]store.Report{
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok",
						VerifyState: "failed", ReplicaIncomplete: "rebuild"},
					{Name: "scratch01", Status: "unreplicated", Active: true},
				}},
			}, now)

		if len(d.Pairs) != 1 {
			t.Fatalf("got %d pairs, want 1", len(d.Pairs))
		}
		if !d.Pairs[0].VerifyFailed() {
			t.Error("the verify failure was dropped with the host: the copy still differs from its source, and nothing else on this console records that")
		}
		if !d.Pairs[0].ReplicaPartial() {
			t.Error("the partial-copy marker was dropped: a half-written replica is still half-written after its agent is revoked")
		}
		if len(d.Unprotected) != 1 || d.Unprotected[0].VM != "scratch01" {
			t.Errorf("Unprotected = %+v, want scratch01 -- a VM nobody replicates does not become protected by its host being withdrawn", d.Unprotected)
		}
	})

	// Re-enrolment: the withdrawn record and the new agent's coexist under one
	// hostname until the old one is cleaned up, and byRef holds one entry per
	// "host:vm". The live observation has to win whichever order they arrive
	// in -- the SOURCE side is where that is visible, because that is the end
	// resolved through the index rather than built directly from a report.
	for _, order := range []string{"revoked first", "live first"} {
		t.Run("a re-enrolled source is observed, not withdrawn ("+order+")", func(t *testing.T) {
			old := store.Agent{ID: "old", Hostname: "hyper01p", LastSeenAt: now.Unix() - 600, Revoked: true, RevokedAt: now.Unix() - 600}
			fresh := store.Agent{ID: "new", Hostname: "hyper01p", LastSeenAt: now.Unix()}
			agents := []store.Agent{old, fresh, {ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()}}
			if order == "live first" {
				agents = []store.Agent{fresh, old, {ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()}}
			}
			d := BuildDashboard(agents,
				map[string]store.Report{
					"old": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 600, Domains: []store.ReportDomain{
						{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok", Active: false},
					}},
					"new": {Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 10, Domains: []store.ReportDomain{
						{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok", Active: true},
					}},
					"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 10, Domains: []store.ReportDomain{
						{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 60},
					}},
				}, now)

			if len(d.Pairs) != 1 {
				t.Fatalf("got %d pairs, want 1", len(d.Pairs))
			}
			p := d.Pairs[0]
			if p.SourceWithdrawn {
				t.Error("the row resolves its source through the withdrawn record although a live agent reports the same domain, so a re-enrolled host reads as gone")
			}
			if !p.SourceActive {
				t.Error("SourceActive came from the withdrawn record rather than the live one -- that is the stale observation winning, and it is what decides split-brain")
			}
		})
	}

	// The half that must not change.
	t.Run("the agents list still shows it, marked revoked", func(t *testing.T) {
		d := BuildDashboard(
			[]store.Agent{{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()}},
			map[string]store.Report{
				"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix() - 30, Domains: []store.ReportDomain{
					{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok"},
				}},
			}, now)

		if len(d.Agents) != 1 {
			t.Fatalf("got %d agents, want 1 -- dropping it loses the record revocation deliberately keeps", len(d.Agents))
		}
		if !d.Agents[0].Revoked {
			t.Error("the agent is listed without its revoked mark")
		}
	})
}
