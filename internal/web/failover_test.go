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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"vmsync-ui/internal/auth"
	"vmsync-ui/internal/store"
)

// failoverFixture is a fleet mid-incident: one pair in split brain, one
// ordinary pair, one domain a fence stopped, and a target whose storage
// cannot hold a second copy.
//
// Deliberately covers every branch the page renders. A fixture where every
// row is healthy would let the template test pass while proving only that
// the empty states work -- the same trap the existing fullFixture guards
// against for the schedule page.
func failoverFixture() ([]store.Agent, map[string]store.Report) {
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix() - 30},
		{ID: "dr", Hostname: "hyper02p", LastSeenAt: now.Unix() - 30},
	}
	reports := map[string]store.Report{
		"src": {
			Hostname: "hyper01p",
			Domains: []store.ReportDomain{
				// Still running, and its target has been promoted: the
				// split brain this page exists to resolve.
				// A fence was armed against it and did NOT stop it: the
				// alarm that exists nowhere in libvirt.
				{
					Name: "web01", Active: true, Role: store.RoleSource,
					ReplicaTargets: []string{"hyper02p:web01"},
					Disks:          []store.ReportDisk{{Path: "/var/lib/libvirt/images/web01.qcow2", AllocatedBytes: 20 << 30}},
					Fenced: &store.ReportFenced{
						FenceID: "fence-web01", State: "failed", AtUnix: now.Unix() - 400,
						PeerRef: "hyper02p:web01", ArmedBy: "alice",
						Error: "the guest did not shut down within 300s",
					},
				},
				// An ordinary source whose target is a plain replica.
				{
					Name: "db01", Active: true, Role: store.RoleSource,
					ReplicaTargets: []string{"hyper02p:db01"},
					Disks:          []store.ReportDisk{{Path: "/var/lib/libvirt/images/db01.qcow2", AllocatedBytes: 5 << 30}},
				},
				// Stopped and paused because a fence stopped it. Identical to
				// an administrative pause in libvirt; only the agent's
				// ledger tells them apart.
				{
					Name: "mail01", Active: false, Role: store.RolePaused,
					ReplicaTargets: []string{"hyper02p:mail01"},
					Fenced: &store.ReportFenced{
						FenceID: "fence-mail01", State: "done", AtUnix: now.Unix() - 1800,
						PeerRef: "hyper02p:mail01", ArmedBy: "alice",
					},
				},
				// Running, and its target was promoted but never started --
				// a failover that stopped half way, or one somebody thought
				// better of. NOT a split brain: only one copy is serving,
				// and calling it one would make the alarm mean "a promotion
				// exists" rather than "two copies are taking writes".
				{
					Name: "app01", Active: true, Role: store.RoleSource,
					ReplicaTargets: []string{"hyper02p:app01"},
				},
			},
			Filesystems: []store.ReportFilesystem{
				{Path: "/var/lib/libvirt/images", TotalBytes: 500 << 30, FreeBytes: 200 << 30},
			},
		},
		"dr": {
			Hostname: "hyper02p",
			Domains: []store.ReportDomain{
				{
					Name: "web01", Active: true, Role: store.RolePromoted,
					ReplicaSource: "hyper01p:web01",
					PromotedFrom:  "hyper01p:web01", PromotedAtUnix: now.Unix() - 600,
					PromotedBy: "alice", PromotionMode: "forced",
					// This promotion armed the fence that then failed above.
					FenceID: "fence-web01", FenceSource: "hyper01p:web01",
					FenceArmedAtUnix: now.Unix() - 600, FenceArmedBy: "alice",
					Disks: []store.ReportDisk{{Path: "/replicas/web01.qcow2", AllocatedBytes: 20 << 30}},
				},
				// A plain replica, and the storage under it has less room
				// than the domain occupies -- so keeping the displaced copy
				// on an inversion would not fit.
				{
					Name: "db01", Active: false, ReplicaSource: "hyper01p:db01",
					Disks: []store.ReportDisk{{Path: "/replicas/db01.qcow2", AllocatedBytes: 5 << 30}},
				},
				{Name: "mail01", Active: false, Role: store.RoleTarget, ReplicaSource: "hyper01p:mail01"},
				{
					Name: "app01", Active: false, Role: store.RolePromoted,
					ReplicaSource: "hyper01p:app01", PromotedFrom: "hyper01p:app01",
					PromotedAtUnix: now.Unix() - 120, PromotedBy: "bob", PromotionMode: "planned",
				},
			},
			Filesystems: []store.ReportFilesystem{
				{Path: "/replicas", TotalBytes: 100 << 30, FreeBytes: 1 << 30},
			},
		},
	}
	return agents, reports
}

// failoverOperationFixture covers every state the operations table renders,
// including the ones with a cancel button and the ones with a reason.
func failoverOperationFixture() []store.OperationRecord {
	done := store.OperationResult{ID: "op-done", State: store.OpStateDone}
	failed := store.OperationResult{
		ID: "op-failed", State: store.OpStateFailed,
		Error:   "exit status 1",
		LogTail: "connecting\nrefusing to promote db01: the target holds no usable replica\n",
	}
	refused := store.OperationResult{
		ID: "op-refused", State: store.OpStateRefused,
		Error: "operation op-refused names peer dr99:db01, but db01's own metadata records hyper01p:db01",
	}
	return []store.OperationRecord{
		// Still awaiting an agent: the only state with a cancel button.
		{Operation: store.Operation{ID: "op-pending", Kind: store.OpPromote, VM: "db01",
			CreatedAtUnix: now.Unix() - 30, CreatedBy: "op", NotAfterUnix: now.Unix() + 600}, AgentID: "dr"},
		// Past its deadline but still published, so the agent refuses it and
		// says so rather than leaving the audit entry open forever.
		{Operation: store.Operation{ID: "op-expired", Kind: store.OpInvert, VM: "mail01",
			CreatedAtUnix: now.Unix() - 3600, CreatedBy: "op", NotAfterUnix: now.Unix() - 60}, AgentID: "src"},
		{Operation: store.Operation{ID: "op-done", Kind: store.OpShutdown, VM: "web01",
			CreatedAtUnix: now.Unix() - 900, CreatedBy: "op"}, AgentID: "src", Result: &done},
		{Operation: store.Operation{ID: "op-failed", Kind: store.OpPromote, VM: "db01",
			CreatedAtUnix: now.Unix() - 1800, CreatedBy: "alice"}, AgentID: "dr", Result: &failed},
		{Operation: store.Operation{ID: "op-refused", Kind: store.OpPromote, VM: "db01",
			CreatedAtUnix: now.Unix() - 2400, CreatedBy: "alice"}, AgentID: "dr", Result: &refused},
		{Operation: store.Operation{ID: "op-cancelled", Kind: store.OpSetRole, VM: "mail01",
			CreatedAtUnix: now.Unix() - 3000, CreatedBy: "op"}, AgentID: "src",
			CancelledAtUnix: now.Unix() - 2900, CancelledBy: "op"},
	}
}

func rowFor(t *testing.T, v FailoverView, host, vm string) FailoverRow {
	t.Helper()
	for _, r := range v.Rows {
		if r.Hostname == host && r.VM == vm {
			return r
		}
	}
	t.Fatalf("no row for %s:%s", host, vm)
	return FailoverRow{}
}

// The cross-reference is the reason this page can exist at all: a source's
// own metadata cannot say that its target has been promoted, and neither
// agent can see the other. Only the control plane hears from both.
func TestFailoverViewCrossReferencesThePeersOwnReport(t *testing.T) {
	agents, reports := failoverFixture()
	v := BuildFailoverView(agents, reports, nil, now)

	src := rowFor(t, v, "hyper01p", "web01")
	if !src.PeerSeen {
		t.Fatal("the peer is reported by another agent and must be seen")
	}
	if src.PeerRole != store.RolePromoted {
		t.Errorf("peer role = %q, want promoted -- read from the PEER's report, not this domain's metadata", src.PeerRole)
	}
	if !src.SplitBrain() {
		t.Error("a running source whose target is promoted and running is a split brain")
	}
	if v.SplitBrainCount != 1 {
		t.Errorf("SplitBrainCount = %d, want 1", v.SplitBrainCount)
	}

	// The healthy pair must NOT be flagged, or the count means nothing.
	db := rowFor(t, v, "hyper01p", "db01")
	if db.SplitBrain() {
		t.Error("a source whose target is an ordinary replica is not a split brain")
	}

	// Neither must a promoted peer that is not RUNNING. Only one copy is
	// serving there, which is the whole distinction: an alarm that fired on
	// this would mean "a promotion exists" rather than "two copies are
	// taking writes", and people would learn to scroll past it.
	app := rowFor(t, v, "hyper01p", "app01")
	if app.PeerRole != store.RolePromoted {
		t.Fatalf("fixture drift: app01's peer role = %q", app.PeerRole)
	}
	if app.PeerActive {
		t.Fatal("fixture drift: app01's promoted peer should be stopped")
	}
	if app.SplitBrain() {
		t.Error("a promoted peer that was never started is not a second live copy")
	}
	if v.SplitBrainCount != 1 {
		t.Errorf("SplitBrainCount = %d after adding a promoted-but-stopped peer, want 1", v.SplitBrainCount)
	}
}

// Which actions a row offers is the safety design of the page. Each case
// here is a specific way of offering something that would be wrong.
func TestFailoverOffersOnlyTheActionsAStateAllows(t *testing.T) {
	agents, reports := failoverFixture()
	v := BuildFailoverView(agents, reports, nil, now)

	t.Run("a running source whose target is promoted can invert and be shut down", func(t *testing.T) {
		r := rowFor(t, v, "hyper01p", "web01")
		if !r.CanInvert() {
			t.Error("inverting is exactly the resolution for this state, and runs on this host")
		}
		if !r.CanShutdown() {
			t.Error("a running domain can always be shut down cleanly")
		}
		if r.CanPromote() {
			t.Error("promoting a SOURCE would ask to overwrite the original with its own replica")
		}
	})

	t.Run("a plain replica can be promoted but not inverted", func(t *testing.T) {
		r := rowFor(t, v, "hyper02p", "db01")
		if !r.CanPromote() {
			t.Error("an ordinary replica is precisely what promotion is for")
		}
		if r.CanInvert() {
			t.Error("there is nothing to invert until a promotion has happened")
		}
		if r.CanShutdown() {
			t.Error("a stopped domain cannot be shut down")
		}
	})

	t.Run("an already-promoted domain is not offered promotion again", func(t *testing.T) {
		r := rowFor(t, v, "hyper02p", "web01")
		if r.CanPromote() {
			t.Error("re-promoting a promoted domain does nothing; invert or set-role is the useful action")
		}
	})

	t.Run("a fenced domain keeps a way back", func(t *testing.T) {
		r := rowFor(t, v, "hyper01p", "mail01")
		if r.Role != store.RolePaused {
			t.Fatalf("fixture drift: role = %q", r.Role)
		}
		if !r.CanSetRole() {
			t.Error("clearing paused is the only way back from a fence, and must always be reachable")
		}
	})
}

// CanPromote's role check, exercised independently of the fixture.
//
// The interesting row is a domain that is BOTH somebody's replica and marked
// source -- a chain, A to B to C, where B receives from A and sends onward to
// C. The IsReplica guard passes there, so the role is the only thing standing
// between an operator and a promotion that would ask vmsync to overwrite a
// live primary with its own copy. A fixture of simple two-host pairs never
// reaches that branch, which is exactly how it would rot unnoticed.
func TestCanPromoteChecksTheRoleAndNotOnlyTheShape(t *testing.T) {
	base := FailoverRow{AgentID: "a", IsReplica: true}
	for _, tc := range []struct {
		name   string
		role   string
		active bool
		want   bool
	}{
		{"no role recorded, as every domain predating the feature carries", "", false, true},
		{"an ordinary target", store.RoleTarget, false, true},
		{"a paused replica", store.RolePaused, false, true},
		{"a source, which promoting would overwrite with its own copy", store.RoleSource, false, false},
		{"a source that happens to be stopped is still a source", store.RoleSource, true, false},
		{"already promoted and serving", store.RolePromoted, true, false},
		// The half-finished failover: vmsync records the promotion before
		// booting, so this is what a crash or a refused start leaves behind.
		// vmsync's own -promote starts it without rewriting the record, and
		// refusing here would leave the console able to begin a failover and
		// unable to finish one.
		{"promoted but never started", store.RolePromoted, false, true},
	} {
		r := base
		r.Role, r.Active = tc.role, tc.active
		if got := r.CanPromote(); got != tc.want {
			t.Errorf("%s: CanPromote() = %v, want %v", tc.name, got, tc.want)
		}
	}

	t.Run("the control says which of the two it is", func(t *testing.T) {
		stalled := FailoverRow{AgentID: "a", IsReplica: true, Role: store.RolePromoted}
		if !stalled.PromoteFinishesAStalledFailover() {
			t.Error("a promoted, stopped domain is a failover to finish, not a promotion to make")
		}
		fresh := FailoverRow{AgentID: "a", IsReplica: true, Role: store.RoleTarget}
		if fresh.PromoteFinishesAStalledFailover() {
			t.Error("an ordinary replica is an ordinary promotion")
		}
	})

	t.Run("a domain that is nobody's replica is never promotable", func(t *testing.T) {
		r := FailoverRow{AgentID: "a", IsSource: true}
		if r.CanPromote() {
			t.Error("there is nothing to promote a domain from")
		}
	})

	t.Run("no agent means no action, whatever the state", func(t *testing.T) {
		r := FailoverRow{IsReplica: true, Role: store.RoleTarget, Active: true}
		if r.CanPromote() || r.CanShutdown() || r.CanSetRole() {
			t.Error("without an enrolled agent there is nothing that could carry an action out")
		}
	})
}

// An inversion whose peer has no enrolled agent succeeds on the hypervisors
// and then jams: the store treats a missing agent for the new source as a
// hard error while processing the very report carrying the outcome. Not
// offering it is the fix.
func TestFailoverDoesNotOfferAnInversionThatWouldJamItsOwnResult(t *testing.T) {
	t.Run("the peer's host has no enrolled agent", func(t *testing.T) {
		agents, reports := failoverFixture()
		// Drop the DR agent while keeping its stored report, which is
		// exactly the shape left behind by revoking an agent or losing a
		// site: the last thing it said is still on file.
		agents = agents[:1]
		v := BuildFailoverView(agents, reports, nil, now)

		r := rowFor(t, v, "hyper01p", "web01")
		if r.PeerSeen {
			t.Fatal("a report from an agent that is no longer enrolled must not count as seeing the peer")
		}
		if r.CanInvert() {
			t.Error("with no enrolled agent on the promoted peer's host, the schedule cannot follow the inversion")
		}
	})

	t.Run("the peer's agent is revoked", func(t *testing.T) {
		agents, reports := failoverFixture()
		agents[1].Revoked = true
		v := BuildFailoverView(agents, reports, nil, now)

		r := rowFor(t, v, "hyper01p", "web01")
		if r.CanInvert() {
			t.Error("a revoked agent cannot carry anything out, so the inversion would jam the same way")
		}
		// And the revoked host's own domains must not appear as rows
		// offering actions of their own.
		for _, row := range v.Rows {
			if row.Hostname == "hyper02p" {
				t.Errorf("a revoked agent's domain %s is still listed", row.VM)
			}
		}
	})

	t.Run("the peer is reported but was never promoted", func(t *testing.T) {
		agents, reports := failoverFixture()
		v := BuildFailoverView(agents, reports, nil, now)
		// db01's target is an ordinary replica: nothing to invert.
		r := rowFor(t, v, "hyper01p", "db01")
		if !r.PeerSeen {
			t.Fatal("fixture drift: db01's target is reported")
		}
		if r.CanInvert() {
			t.Error("inverting a pair nobody has failed over would reverse a healthy direction")
		}
	})
}

// Rows needing a decision come first. During an incident nobody should have
// to scroll to find the VM that is running twice.
func TestFailoverPutsWhatNeedsADecisionFirst(t *testing.T) {
	agents, reports := failoverFixture()
	v := BuildFailoverView(agents, reports, nil, now)

	if len(v.Rows) == 0 {
		t.Fatal("no rows")
	}
	if !v.Rows[0].SplitBrain() {
		t.Errorf("the first row is %s:%s, but the split brain should outrank everything",
			v.Rows[0].Hostname, v.Rows[0].VM)
	}
	// And every row that needs a decision precedes every row that does not.
	seenQuiet := false
	for _, r := range v.Rows {
		if !r.NeedsDecision() {
			seenQuiet = true
			continue
		}
		if seenQuiet {
			t.Errorf("%s:%s needs a decision but sorts after a quiet row", r.Hostname, r.VM)
		}
	}
}

// The disk figures exist to answer one question: can an inversion afford to
// keep the displaced copy rather than deleting it. Answering it wrongly in
// the reassuring direction is how somebody fills a filesystem.
func TestFailoverReportsWhetherKeepingTheOldCopyFits(t *testing.T) {
	agents, reports := failoverFixture()
	v := BuildFailoverView(agents, reports, nil, now)

	tight := rowFor(t, v, "hyper02p", "db01")
	if !tight.FreeKnown {
		t.Fatal("the filesystem behind this domain's disks was reported and must be found")
	}
	if tight.KeepingOldDisksFits() {
		t.Errorf("5 GiB of disks with 1 GiB free does not fit a second copy (free=%d allocated=%d)",
			tight.FreeBytes, tight.AllocatedBytes)
	}

	roomy := rowFor(t, v, "hyper01p", "db01")
	if !roomy.KeepingOldDisksFits() {
		t.Error("5 GiB of disks with 200 GiB free plainly fits")
	}

	// Unknown storage must not raise the warning. Warning on a number
	// nobody has is how people learn to ignore warnings.
	unknown := rowFor(t, v, "hyper01p", "mail01")
	if unknown.FreeKnown {
		t.Fatal("fixture drift: mail01 has no disks and no filesystem")
	}
	if !unknown.KeepingOldDisksFits() {
		t.Error("unknown storage must not be reported as not fitting")
	}
	if unknown.Free() != "unknown" {
		t.Errorf("Free() = %q, want %q", unknown.Free(), "unknown")
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{20 << 30, "20.0 GiB"},
		{1536 << 20, "1.5 GiB"},
		{3 << 40, "3.0 TiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Rendering without error is not the same as rendering the controls. This
// asserts the page an admin actually gets: the forms, the fence checkbox,
// the cancel button, and the storage figures.
func TestFailoverPageRendersItsControls(t *testing.T) {
	s := testServer(t)
	agents, reports := failoverFixture()
	v := BuildFailoverView(agents, reports, failoverOperationFixture(), now)

	var buf strings.Builder
	err := s.tpl.ExecuteTemplate(&buf, "failover.html", pageData{
		User:     auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:   "failover",
		Failover: v,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		`action="/failover/operation"`,
		`value="promote"`,
		`value="invert"`,
		`value="shutdown-domain"`,
		`value="set-role"`,
		`name="arm_fence"`, // the fencing hook, the whole reason this page can arm one
		`action="/failover/cancel"`,
		`name="csrf"`,
		"SPLIT BRAIN", // the condition that brought somebody here
		"20.0 GiB",    // the disk figures, finally rendered somewhere
		"not enough room to keep the old copy",
		// The fence facts that exist nowhere in libvirt and could not be
		// derived here from anything else.
		"stopped by vmsync",           // a fenced VM, not an administrative pause
		"did not stop this domain",    // a fence that failed
		"will NOT be retried",         // and nothing is going to try again
		"armed a fence against",       // what a promotion authorised
		"the guest did not shut down", // the agent's own reason, carried through
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the rendered page is missing %q", want)
		}
	}

	// The refused option must not be offered, or the handler's refusal is a
	// dead end an operator reaches by following the page's own suggestion.
	if strings.Contains(html, `<option value="promoted"`) {
		t.Error("set-role must not offer `promoted`: promotion has preconditions that writing the role would skip")
	}
}

// A reader sees the state and none of the buttons.
func TestFailoverPageOffersAReaderNothingToClick(t *testing.T) {
	s := testServer(t)
	agents, reports := failoverFixture()

	var buf strings.Builder
	err := s.tpl.ExecuteTemplate(&buf, "failover.html", pageData{
		User:     auth.User{Username: "bob", Role: auth.RoleReadOnly},
		Active:   "failover",
		Failover: BuildFailoverView(agents, reports, failoverOperationFixture(), now),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, `action="/failover/operation"`) || strings.Contains(html, `action="/failover/cancel"`) {
		t.Error("a read-only account is shown a form it cannot submit")
	}
	// But the state itself is not hidden: seeing a split brain is not an
	// action, and hiding it from the people most likely to be watching the
	// board would be the wrong half to withhold.
	if !strings.Contains(html, "SPLIT BRAIN") {
		t.Error("a reader must still see that a VM is running in two places")
	}
}

// Nothing prunes the operation store, so an estate that has been running for
// years accumulates a record per failover forever. The page caps what it
// renders -- but a PENDING operation must never be capped away: it is the
// only row with a Cancel button, and one hidden behind a limit is one
// blocking its VM with no way to see or clear it.
func TestOperationsListCapsHistoryButNeverHidesAPendingOne(t *testing.T) {
	agents, reports := failoverFixture()

	var ops []store.OperationRecord
	// Newest first, as the store returns them. Enough finished ones to
	// overflow the cap several times over.
	ops = append(ops, store.OperationRecord{
		Operation: store.Operation{ID: "still-pending", Kind: store.OpPromote, VM: "db01",
			CreatedAtUnix: now.Unix() - 10, CreatedBy: "op", NotAfterUnix: now.Unix() + 600},
		AgentID: "dr",
	})
	for i := 0; i < operationsShown*3; i++ {
		res := store.OperationResult{ID: "old-" + strconv.Itoa(i), State: store.OpStateDone}
		ops = append(ops, store.OperationRecord{
			Operation: store.Operation{ID: "old-" + strconv.Itoa(i), Kind: store.OpShutdown, VM: "web01",
				CreatedAtUnix: now.Unix() - int64(100+i), CreatedBy: "op"},
			AgentID: "src", Result: &res,
		})
	}
	// And one more pending, buried far below the cap by age.
	ops = append(ops, store.OperationRecord{
		Operation: store.Operation{ID: "old-but-pending", Kind: store.OpInvert, VM: "mail01",
			CreatedAtUnix: now.Unix() - 99999, CreatedBy: "op", NotAfterUnix: now.Unix() + 600},
		AgentID: "src",
	})

	v := BuildFailoverView(agents, reports, ops, now)

	if len(v.Operations) != operationsShown+2 {
		t.Errorf("rendered %d operations, want %d finished plus both pending", len(v.Operations), operationsShown)
	}
	if v.OlderHidden != operationsShown*3-operationsShown {
		t.Errorf("OlderHidden = %d, want %d", v.OlderHidden, operationsShown*2)
	}
	if v.Pending != 2 {
		t.Errorf("Pending = %d, want 2", v.Pending)
	}

	shown := map[string]bool{}
	for _, o := range v.Operations {
		shown[o.ID] = true
	}
	for _, id := range []string{"still-pending", "old-but-pending"} {
		if !shown[id] {
			t.Errorf("%s was capped away; a pending operation blocks its VM and must always be visible and cancellable", id)
		}
	}
	// The most recent finished one survives, the oldest does not.
	if !shown["old-0"] {
		t.Error("the most recent finished operation should be listed")
	}
	if shown["old-"+strconv.Itoa(operationsShown*3-1)] {
		t.Error("the oldest finished operation should have been dropped, not the newest")
	}
}

// The gap this closes. From metadata alone a fenced VM and one an operator
// paused are both just `paused`, and a fence that FAILED leaves no trace in
// libvirt at all -- only a VM still running beside a promoted copy, which is
// indistinguishable from a failover nobody has got to yet. Both facts exist
// only in the agent's own ledger.
func TestFencedIsToldApartFromAnAdministrativePause(t *testing.T) {
	fenced := FailoverRow{
		Role: store.RolePaused, Active: false,
		Fenced: &store.ReportFenced{
			FenceID: "f1", State: "done", AtUnix: now.Unix() - 300,
			PeerRef: "hyper02p:web01", ArmedBy: "alice",
		},
	}
	paused := FailoverRow{Role: store.RolePaused, Active: false}

	if !fenced.WasFenced() {
		t.Error("a domain a fence stopped must be identifiable as such")
	}
	if paused.WasFenced() {
		t.Error("a domain nobody fenced must not claim to have been")
	}
	if fenced.FenceState() != "fenced" || paused.FenceState() != "" {
		t.Errorf("FenceState: fenced=%q paused=%q", fenced.FenceState(), paused.FenceState())
	}
	// Both are `paused`, which is exactly why the extra fact is needed.
	if fenced.Role != paused.Role {
		t.Fatal("fixture drift: the whole point is that the roles are identical")
	}
}

func TestAFailedFenceIsAnAlarmOnlyWhileTheDomainStillRuns(t *testing.T) {
	failed := &store.ReportFenced{
		FenceID: "f2", State: "failed", AtUnix: now.Unix() - 60,
		PeerRef: "hyper02p:web01", Error: "the guest did not shut down in time",
	}

	running := FailoverRow{Role: store.RoleSource, Active: true, Fenced: failed}
	if !running.FenceFailed() {
		t.Error("a fence that failed against a still-running domain is a live split brain")
	}
	if running.FenceState() != "fence failed" {
		t.Errorf("FenceState() = %q", running.FenceState())
	}
	if !running.NeedsDecision() {
		t.Error("a failed fence needs a person and must sort with what needs attention")
	}

	// Somebody shut it down by hand afterwards. The ledger entry is latched
	// forever by design, so without the Active requirement this row would
	// keep shouting about a situation that no longer exists.
	resolved := FailoverRow{Role: store.RoleSource, Active: false, Fenced: failed}
	if resolved.FenceFailed() {
		t.Error("a failed fence whose domain is now stopped is resolved, and must stop alarming")
	}

	// An interrupted fence -- the agent died mid-shutdown -- is not a
	// success and must alarm the same way.
	interrupted := FailoverRow{Role: store.RoleSource, Active: true,
		Fenced: &store.ReportFenced{FenceID: "f3", State: "running"}}
	if !interrupted.FenceFailed() {
		t.Error("a fence interrupted mid-shutdown left the domain running and is not a success")
	}
	if interrupted.WasFenced() {
		t.Error("only a completed fence counts as having fenced")
	}
}

// A failed fence outranks everything, including the inferred split brain:
// something already tried to resolve this and could not, and nothing will
// try again.
func TestAFailedFenceSortsToTheTop(t *testing.T) {
	agents, reports := failoverFixture()
	// db01's source on hyper01p is a quiet, healthy row. Give it a failed
	// fence and it should overtake the split brain's neighbours.
	src := reports["src"]
	doms := append([]store.ReportDomain(nil), src.Domains...)
	for i := range doms {
		if doms[i].Name == "db01" {
			doms[i].Fenced = &store.ReportFenced{
				FenceID: "f9", State: "failed", AtUnix: now.Unix() - 30,
				PeerRef: "hyper02p:db01", Error: "the guest did not shut down in time",
			}
		}
	}
	src.Domains = doms
	reports["src"] = src

	v := BuildFailoverView(agents, reports, nil, now)
	// Two: the fixture's own failed fence on web01, plus the one just added.
	if v.FenceFailedCount != 2 {
		t.Fatalf("FenceFailedCount = %d, want 2", v.FenceFailedCount)
	}
	// web01 is BOTH a split brain and a failed fence -- one VM in trouble,
	// described two ways. The headline counts domains, so adding the two
	// figures would send an operator looking for a VM that does not exist.
	if v.SplitBrainCount != 1 {
		t.Fatalf("SplitBrainCount = %d, want 1", v.SplitBrainCount)
	}
	if v.UrgentCount != 2 {
		t.Errorf("UrgentCount = %d, want 2 (web01 counted once, plus db01), not %d",
			v.UrgentCount, v.SplitBrainCount+v.FenceFailedCount)
	}
	db := rowFor(t, v, "hyper01p", "db01")
	if !db.FenceFailed() {
		t.Fatal("the failed fence did not reach the row")
	}

	// db01 was otherwise the quietest row on the page -- a healthy source
	// with a healthy target. The failed fence alone must lift it above every
	// row that is merely promoted or paused.
	urgent := 0
	for _, r := range v.Rows {
		if !r.SplitBrain() && !r.FenceFailed() {
			break
		}
		urgent++
	}
	if urgent < 2 {
		t.Errorf("only %d urgent rows sort first; a failed fence must not be buried", urgent)
	}
	for i, r := range v.Rows {
		if r.Hostname == "hyper01p" && r.VM == "db01" {
			if i >= urgent {
				t.Errorf("db01 sorts at %d, outside the %d urgent rows", i, urgent)
			}
			break
		}
	}
}

// The armed token travels too, so a promoted row can say whether its
// promotion authorised stopping anything. Its absence is the drill.
func TestAPromotedRowShowsWhetherItArmedAFence(t *testing.T) {
	agents, reports := failoverFixture()
	dr := reports["dr"]
	doms := append([]store.ReportDomain(nil), dr.Domains...)
	for i := range doms {
		if doms[i].Name == "web01" {
			doms[i].FenceSource = "hyper01p:web01"
			doms[i].FenceID = "f7"
			doms[i].FenceArmedAtUnix = now.Unix() - 600
			doms[i].FenceArmedBy = "alice"
		}
	}
	dr.Domains = doms
	reports["dr"] = dr

	v := BuildFailoverView(agents, reports, nil, now)

	armed := rowFor(t, v, "hyper02p", "web01")
	if armed.ArmedFenceSource != "hyper01p:web01" {
		t.Errorf("ArmedFenceSource = %q", armed.ArmedFenceSource)
	}
	if armed.ArmedFenceArmedBy != "alice" || armed.ArmedFenceAt() == "" {
		t.Errorf("the attribution did not survive: by=%q at=%q", armed.ArmedFenceArmedBy, armed.ArmedFenceAt())
	}

	// app01 was promoted without arming anything -- a drill.
	drill := rowFor(t, v, "hyper02p", "app01")
	if drill.ArmedFenceSource != "" {
		t.Error("a promotion that armed no fence must not appear to have armed one")
	}
}

// --- issuing operations ---------------------------------------------------

// The join this page exists for: what an admin clicks has to come back out
// of the endpoint the agent polls, addressed to the right agent.
func TestPromoteReachesTheAgentConfig(t *testing.T) {
	s := testServer(t)
	agents, reports := failoverFixture()
	for _, a := range agents {
		if err := s.Store.SaveReport(a.ID, reports[a.ID]); err != nil {
			t.Fatalf("save report: %v", err)
		}
	}

	rec := post(t, s, "/failover/operation", url.Values{
		"kind":      {"promote"},
		"agent_id":  {"dr"},
		"vm":        {"db01"},
		"peer_host": {"hyper01p"},
		"peer_vm":   {"db01"},
		"mode":      {"forced"},
		"start":     {"on"},
		"arm_fence": {"on"},
	})
	if rec.Code != 303 {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}

	cfg, _, err := s.Store.AgentConfigFor("dr")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 1 {
		t.Fatalf("the agent is published %d operations, want 1", len(cfg.Operations))
	}
	op := cfg.Operations[0]
	if op.Kind != store.OpPromote || op.VM != "db01" {
		t.Errorf("operation = %s on %s, want promote on db01", op.Kind, op.VM)
	}
	if op.Mode != "forced" || !op.StartVM {
		t.Errorf("mode=%q start=%v, want forced/true", op.Mode, op.StartVM)
	}
	if !op.ArmFence {
		t.Error("the fence checkbox must reach the agent, or promoting from this console can never stop the old source")
	}
	if op.Force {
		t.Error("force was not ticked and must not be set")
	}
	if op.ID == "" || op.NotAfterUnix == 0 {
		t.Error("an operation needs an id and a deadline")
	}
	// Nothing should be published to the OTHER agent.
	other, _, err := s.Store.AgentConfigFor("src")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(other.Operations) != 0 {
		t.Errorf("the source agent was published %d operations, want 0", len(other.Operations))
	}
}

// Arming a fence is opt-in the whole way down. A drill is a promotion too,
// and one that stopped production would be worse than the split brain it
// was rehearsing for.
func TestPromoteWithoutTheFenceCheckboxArmsNothing(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"planned"},
	})
	if rec.Code != 303 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg, _, err := s.Store.AgentConfigFor("dr")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 1 {
		t.Fatalf("got %d operations, want 1", len(cfg.Operations))
	}
	if cfg.Operations[0].ArmFence {
		t.Error("an unticked checkbox sends nothing, and must not arm a fence")
	}
}

// Setting role=promoted by hand would reach the recorded state of a
// promotion having verified none of its preconditions.
func TestSetRoleRefusesToWritePromotedByHand(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"set-role"}, "agent_id": {"dr"}, "vm": {"db01"}, "role": {"promoted"},
	})
	if rec.Code != 303 {
		t.Fatalf("status = %d, want a redirect carrying the refusal", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("expected an error in the redirect, got %q", loc)
	}
	cfg, _, err := s.Store.AgentConfigFor("dr")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("nothing should have been published")
	}
}

func TestSetRoleAcceptsTheWayBackFromAFence(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"set-role"}, "agent_id": {"src"}, "vm": {"mail01"}, "role": {"target"},
	})
	if rec.Code != 303 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg, _, err := s.Store.AgentConfigFor("src")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 1 || cfg.Operations[0].Mode != store.RoleTarget {
		t.Fatalf("got %+v, want one set-role to target", cfg.Operations)
	}
}

// One in flight per VM. Two promotions of one domain is not something an
// operator should be able to create by clicking twice on a slow page.
func TestASecondOperationOnTheSameVMIsRefused(t *testing.T) {
	s := testServer(t)
	first := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"},
	})
	if first.Code != 303 {
		t.Fatalf("first: %d", first.Code)
	}
	second := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"},
	})
	if loc := second.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("the second should be refused, got %q", loc)
	}
	cfg, _, err := s.Store.AgentConfigFor("dr")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 1 {
		t.Errorf("got %d operations, want 1", len(cfg.Operations))
	}
}

// Cancelling is the off switch an armed promotion otherwise has only a
// 15-minute deadline for.
func TestCancelStopsAnOperationBeingPublished(t *testing.T) {
	s := testServer(t)
	if rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"},
	}); rec.Code != 303 {
		t.Fatalf("issue: %d", rec.Code)
	}
	ops, err := s.Store.Operations()
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations: %v (%d)", err, len(ops))
	}

	if rec := post(t, s, "/failover/cancel", url.Values{"id": {ops[0].ID}}); rec.Code != 303 {
		t.Fatalf("cancel: %d — %s", rec.Code, rec.Body.String())
	}
	cfg, _, err := s.Store.AgentConfigFor("dr")
	if err != nil {
		t.Fatalf("agent config: %v", err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("a cancelled operation must stop being published")
	}
	// And the same VM is free again, which is the point of cancelling.
	if rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"},
	}); rec.Code != 303 {
		t.Fatalf("re-issue after cancel: %d", rec.Code)
	}
}

// A shutdown carries its timeout, resolved when it is issued.
//
// Resolved here rather than by the agent so the instruction means the same
// thing whenever it runs: an operation that silently meant 300 seconds in
// March and 900 in April, because somebody edited a setting in between, is
// not one anybody can audit.
func TestAShutdownCarriesTheResolvedTimeout(t *testing.T) {
	t.Run("the VM's own override", func(t *testing.T) {
		s := testServer(t)
		set, _ := s.Store.Settings()
		set.ShutdownTimeoutSec = 300
		if err := s.Store.SetSettings(set); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}
		if err := s.Store.SetScheduleEntry("src", store.ScheduleEntry{
			VM: "db01", IntervalSeconds: 900, Enabled: true, ShutdownTimeoutSec: 1200,
		}); err != nil {
			t.Fatalf("SetScheduleEntry: %v", err)
		}

		if rec := post(t, s, "/failover/operation", url.Values{
			"kind": {"shutdown-domain"}, "agent_id": {"src"}, "vm": {"db01"},
		}); rec.Code != 303 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		cfg, _, err := s.Store.AgentConfigFor("src")
		if err != nil {
			t.Fatalf("AgentConfigFor: %v", err)
		}
		if len(cfg.Operations) != 1 {
			t.Fatalf("got %d operations, want 1", len(cfg.Operations))
		}
		if cfg.Operations[0].ShutdownTimeoutSec != 1200 {
			t.Errorf("the operation carries %ds, want the VM's own 1200",
				cfg.Operations[0].ShutdownTimeoutSec)
		}
	})

	t.Run("the estate default when the VM has none", func(t *testing.T) {
		s := testServer(t)
		set, _ := s.Store.Settings()
		set.ShutdownTimeoutSec = 600
		if err := s.Store.SetSettings(set); err != nil {
			t.Fatalf("SetSettings: %v", err)
		}

		if rec := post(t, s, "/failover/operation", url.Values{
			"kind": {"shutdown-domain"}, "agent_id": {"src"}, "vm": {"nowhere01"},
		}); rec.Code != 303 {
			t.Fatalf("status = %d", rec.Code)
		}
		cfg, _, err := s.Store.AgentConfigFor("src")
		if err != nil {
			t.Fatalf("AgentConfigFor: %v", err)
		}
		if cfg.Operations[0].ShutdownTimeoutSec != 600 {
			t.Errorf("the operation carries %ds, want the estate default 600",
				cfg.Operations[0].ShutdownTimeoutSec)
		}
	})

	t.Run("never zero, which the agent would read as no opinion", func(t *testing.T) {
		s := testServer(t)
		if rec := post(t, s, "/failover/operation", url.Values{
			"kind": {"shutdown-domain"}, "agent_id": {"src"}, "vm": {"db01"},
		}); rec.Code != 303 {
			t.Fatalf("status = %d", rec.Code)
		}
		cfg, _, err := s.Store.AgentConfigFor("src")
		if err != nil {
			t.Fatalf("AgentConfigFor: %v", err)
		}
		if cfg.Operations[0].ShutdownTimeoutSec <= 0 {
			t.Error("a shutdown operation must always name a timeout, so the agent never has to guess")
		}
	})
}

// The estate defaults form, which is also the only way to set the value a
// fence falls back on.
func TestEstateDefaultsCanBeSavedAndAreValidated(t *testing.T) {
	t.Run("saved and served", func(t *testing.T) {
		s := testServer(t)
		if rec := post(t, s, "/schedule/settings", url.Values{
			"shutdown_timeout_sec": {"900"}, "max_concurrent_syncs": {"8"},
		}); rec.Code != 303 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		set, err := s.Store.Settings()
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if set.ShutdownTimeoutSec != 900 || set.MaxConcurrentSyncs != 8 {
			t.Errorf("saved settings = %+v, want 900/8", set)
		}
		// The protocol timings must survive a form that does not mention
		// them: blanking a poll interval here would be a silent, estate-wide
		// change to how quickly anything reaches a hypervisor.
		if set.ReportIntervalSeconds == 0 || set.PollWaitSeconds == 0 {
			t.Errorf("saving defaults blanked the poll/report intervals: %+v", set)
		}
	})

	for _, tc := range []struct {
		name, timeout, concurrent string
	}{
		{"a timeout too short to be actioned", "5", "4"},
		{"a timeout long enough to wedge a failover", "7200", "4"},
		{"zero, which is inherit and so meaningless as the thing inherited FROM", "0", "4"},
		{"not a number", "soon", "4"},
		{"concurrency of zero would stop the estate", "300", "0"},
		{"concurrency past the agent's own clamp", "300", "999"},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			s := testServer(t)
			before, _ := s.Store.Settings()
			rec := post(t, s, "/schedule/settings", url.Values{
				"shutdown_timeout_sec": {tc.timeout}, "max_concurrent_syncs": {tc.concurrent},
			})
			if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
				t.Fatalf("expected a refusal, got %q", loc)
			}
			after, _ := s.Store.Settings()
			if after.ShutdownTimeoutSec != before.ShutdownTimeoutSec ||
				after.MaxConcurrentSyncs != before.MaxConcurrentSyncs {
				t.Errorf("a refused save changed the settings anyway: %+v -> %+v", before, after)
			}
		})
	}
}

// Every action is gated. Two independent gates, and both are checked here
// because either alone would be enough to lose: a signed-in admin without a
// valid form token is the CSRF case, and no session at all is the ordinary
// unauthenticated one.
func TestFailoverActionsRefuseTheUngated(t *testing.T) {
	form := url.Values{"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"}}

	for _, path := range []string{"/failover/operation", "/failover/cancel"} {
		t.Run(path+" without a session", func(t *testing.T) {
			s := testServer(t)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			mux := http.NewServeMux()
			s.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusSeeOther && !strings.Contains(rec.Header().Get("Location"), "/login") {
				t.Errorf("accepted an unauthenticated submission (redirected to %q)", rec.Header().Get("Location"))
			}
			if ops, err := s.Store.Operations(); err != nil || len(ops) != 0 {
				t.Errorf("an unauthenticated request created %d operations", len(ops))
			}
		})

		t.Run(path+" with a bad form token", func(t *testing.T) {
			s := testServer(t)
			cookie, _ := signIn(t, s)
			bad := url.Values{}
			for k, v := range form {
				bad[k] = v
			}
			bad.Set("csrf", "not-the-token")
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(bad.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(cookie)
			mux := http.NewServeMux()
			s.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for a bad CSRF token", rec.Code)
			}
			if ops, err := s.Store.Operations(); err != nil || len(ops) != 0 {
				t.Errorf("a request with a bad form token created %d operations", len(ops))
			}
		})
	}
}

func TestEveryIssuedOperationIsAudited(t *testing.T) {
	s := testServer(t)
	if rec := post(t, s, "/failover/operation", url.Values{
		"kind": {"promote"}, "agent_id": {"dr"}, "vm": {"db01"}, "mode": {"forced"}, "arm_fence": {"on"},
	}); rec.Code != 303 {
		t.Fatalf("issue: %d", rec.Code)
	}
	entries, err := s.Store.Audit()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == store.OpPromote && e.Target == "db01" {
			found = true
			if !strings.Contains(e.Detail, "fence=true") {
				t.Errorf("the audit detail should record that a fence was armed, got %q", e.Detail)
			}
			if e.Actor != "op" {
				t.Errorf("actor = %q, want op", e.Actor)
			}
		}
	}
	if !found {
		t.Error("issuing a promotion must leave an audit entry")
	}
}
