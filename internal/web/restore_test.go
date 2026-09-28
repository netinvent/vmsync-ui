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
	"net/url"
	"strings"
	"testing"
	"time"

	"vmsync-ui/internal/store"
)

// A restore is the most destructive thing this page can ask for: it replaces
// a replica's disks. The predicate deciding whether to offer it is therefore
// asserted over the whole matrix rather than by example.

func restorableRow() FailoverRow {
	return FailoverRow{
		AgentID:   "tgt",
		VM:        "web01",
		Role:      store.RoleTarget,
		IsReplica: true,
		RestorePoints: []store.ReportRestorePoint{
			{Tag: "1756041600-vmsync-cpt-000042", TakenAtUnix: 1756041600, CheckpointAtUnix: 1756041000, Verify: "passed", Disks: []string{"web01.qcow2"}},
		},
	}
}

func TestCanRestore(t *testing.T) {
	if !restorableRow().CanRestore() {
		t.Fatal("a shut-off replica with restore points must be restorable")
	}

	for name, mutate := range map[string]func(*FailoverRow){
		// Replacing the disks under a running guest. vmsync refuses it; so
		// should the page, rather than offering a button that fails.
		"running": func(r *FailoverRow) { r.Active = true },
		// Live data a restore would overwrite with an old replica's. Both
		// are refused by vmsync's own gate; this mirrors it so the refusal
		// arrives before the click.
		"is the source of its pair": func(r *FailoverRow) { r.Role = store.RoleSource },
		"was failed over to":        func(r *FailoverRow) { r.Role = store.RolePromoted },
		// Nothing to offer. A control opening onto an empty list is worse
		// than no control.
		"has no restore points": func(r *FailoverRow) { r.RestorePoints = nil },
		// No agent to carry it out.
		"has no agent": func(r *FailoverRow) { r.AgentID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			row := restorableRow()
			mutate(&row)
			if row.CanRestore() {
				t.Errorf("a domain that %s must not be offered a rollback", name)
			}
		})
	}
}

// paused is the COMMON case, not an edge one: a restore leaves the replica
// paused, so a first choice that turns out to be wrong must be followable by
// a second. This is also where the restore gate deliberately differs from the
// sync gate, which refuses paused.
func TestCanRestoreAllowsAPausedReplica(t *testing.T) {
	row := restorableRow()
	row.Role = store.RolePaused
	if !row.CanRestore() {
		t.Fatal("a paused replica must still be restorable, or an operator gets exactly one attempt")
	}
}

func TestCanReinitIsOfferedOnTheSource(t *testing.T) {
	// A reinit is a SYNC, so it runs where the schedule and the credentials
	// are: the source's agent. Offering it on the replica's row would issue
	// it to a host with no schedule entry for the pair.
	src := FailoverRow{AgentID: "src", VM: "web01", IsSource: true}
	if !src.CanReinit() {
		t.Fatal("a source with an agent must be able to be resynced in full")
	}
	replica := FailoverRow{AgentID: "tgt", VM: "web01", IsReplica: true}
	if replica.CanReinit() {
		t.Fatal("a reinit must not be offered on the replica's row -- it runs on the source")
	}
}

// The age shown must be measured from the instant the CONTENTS correspond to,
// not from when the copy finished. A checkpoint is taken before any data
// moves, so everything written from then on belongs to the next one --
// measuring from the end understates how far back a rollback goes by the whole
// duration of that sync, which over a WAN is hours, and always in the
// direction that makes the copy look fresher than it is.
func TestRestorePointViewMeasuresFromTheCheckpointNotTheCopy(t *testing.T) {
	row := restorableRow()
	row.RestorePoints[0].CheckpointAtUnix = 1756041000
	row.RestorePoints[0].TakenAtUnix = 1756041600 // ten minutes later

	views := row.RestorePointViews()
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	wantCheckpoint := time.Unix(1756041000, 0).UTC().Format("2006-01-02 15:04 UTC")
	wantCopy := time.Unix(1756041600, 0).UTC().Format("2006-01-02 15:04 UTC")
	if views[0].TakenAt != wantCheckpoint {
		t.Errorf("TakenAt = %q, want the checkpoint instant %q (it showed %q, when the copy finished)",
			views[0].TakenAt, wantCheckpoint, wantCopy)
	}
}

func TestRestorePointViewFallsBackWhenTheSidecarPredatesCheckpointAt(t *testing.T) {
	row := restorableRow()
	row.RestorePoints[0].CheckpointAtUnix = 0
	views := row.RestorePointViews()
	if views[0].TakenAt == "" {
		t.Fatal("a sidecar with no checkpoint_at must still show when it was taken")
	}
}

// "not-run" is the ORDINARY state -- copies are taken before verification
// runs -- so an operator must be told which of these was ever actually
// checked, in words. Showing the bare term would make "never checked" and
// "checked and clean" look equally reassuring at the worst possible moment.
func TestRestorePointCaveatsDistinguishCheckedFromUnchecked(t *testing.T) {
	row := restorableRow()
	row.RestorePoints = []store.ReportRestorePoint{
		{Tag: "3-c", TakenAtUnix: 3, Verify: "passed"},
		{Tag: "2-c", TakenAtUnix: 2, Verify: "not-run"},
		{Tag: "1-c", TakenAtUnix: 1, Verify: "failed"},
		{Tag: "0-c", TakenAtUnix: 0, Incomplete: true},
	}
	views := row.RestorePointViews()

	if !strings.Contains(views[0].Caveat, "matched") {
		t.Errorf("a passed copy reads %q", views[0].Caveat)
	}
	if !strings.Contains(views[1].Caveat, "never compared") || !strings.Contains(views[1].Caveat, "usual state") {
		t.Errorf("an unverified copy must say both that it was never compared AND that this is normal, got %q", views[1].Caveat)
	}
	if !strings.Contains(views[2].Caveat, "MISMATCH") {
		t.Errorf("a failed copy must be unmissable, got %q", views[2].Caveat)
	}
	if !strings.Contains(views[3].Caveat, "could not be read") {
		t.Errorf("an unreadable record reads %q", views[3].Caveat)
	}
}

// A tag is the one operation parameter that can cease to exist between the
// page rendering and the button being pressed -- retention prunes on every
// sync. Refusing here costs a reload; accepting would burn the operation ID
// permanently, because a kind the agent refuses can never be retried with the
// same ID.
func TestPostRestoreRefusesATagTheAgentNeverReported(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/failover/operation", url.Values{
		"kind":     {store.OpRestore},
		"agent_id": {"tgt"},
		"vm":       {"web01"},
		"tag":      {"1756041600-vmsync-cpt-000042"},
	})

	// No report was ever stored for that agent, so the tag cannot be known.
	// The operator gets an explanation rather than a burned operation.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect carrying the explanation", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("redirect %q carries no error; an unknown tag must be explained, not accepted", loc)
	}
}

func TestPostRestoreRequiresATag(t *testing.T) {
	s := testServer(t)
	rec := post(t, s, "/failover/operation", url.Values{
		"kind":     {store.OpRestore},
		"agent_id": {"tgt"},
		"vm":       {"web01"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatalf("a restore with no restore point selected must be refused, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The restore record is what survives a promotion. Once promoted, role=paused
// is overwritten and every other trace of a rollback is ambiguous with an
// ordinary lagging replica -- so this is the only thing left that explains an
// unusually wide data-loss window.
func TestAPromotedRowStillShowsItWasRolledBack(t *testing.T) {
	row := FailoverRow{
		AgentID: "tgt", VM: "web01", Role: store.RolePromoted,
		RestoredFrom: "1756041600-vmsync-cpt-000042", RestoredAtUnix: 1756900000, RestoredBy: "alice",
	}
	if !row.WasRestored() {
		t.Fatal("a promoted domain that was rolled back no longer says so")
	}
	if row.RestoredAt() == "" {
		t.Error("the rollback has no time")
	}
}

func TestRestoredAtIsWhenTheRollbackHappenedNotWhenTheCopyWasTaken(t *testing.T) {
	// Different instants, and the gap between them is usually the interesting
	// part of an incident timeline.
	row := restorableRow()
	row.RestoredAtUnix = 1756900000 // the rollback
	row.LastSyncUnix = 1756041600   // the copy it used
	if row.RestoredAt() == row.ContentsAsOf() {
		t.Error("the rollback time and the content age render identically")
	}
	if !strings.Contains(row.ContentsAsOf(), "old") {
		t.Errorf("ContentsAsOf should say how old the data is, got %q", row.ContentsAsOf())
	}
}

// The number a promotion decision turns on. inventory.Assess deliberately
// reports no staleness for a promoted or paused domain -- correctly, since a
// growing last-sync age is expected there -- which means the staleness column
// shows a dash for exactly the rows an operator stares at during a failover.
func TestContentsAsOfIsShownForRolesThatHaveNoStalenessAge(t *testing.T) {
	for _, role := range []string{store.RolePromoted, store.RolePaused} {
		row := FailoverRow{AgentID: "tgt", VM: "web01", Role: role, LastSyncUnix: 1756041600}
		if row.ContentsAsOf() == "" {
			t.Errorf("role %q shows no content age, so how old the copy is would be invisible", role)
		}
	}
}

func TestContentsAsOfIsEmptyWhenNothingIsKnown(t *testing.T) {
	// Never synced. An invented date would be worse than a blank.
	if got := (FailoverRow{}).ContentsAsOf(); got != "" {
		t.Errorf("ContentsAsOf = %q for a domain that never synced, want empty", got)
	}
}

// --- a copy that has served live -----------------------------------------

// TestARestoreIsWithheldFromACopyThatServedLive is the hole in TestCanRestore
// above, and the reason it was not visible there: every refusal in that matrix
// is keyed on the ROLE, and the role is rewritten by the very act that leads an
// operator here.
//
// "was failed over to" is refused because Role is `promoted`. But a promoted
// copy is shut down before anything else is done with it, and a clean shutdown
// records `paused` -- which the test two functions down asserts must stay
// restorable, because for an ordinary replica it must. So the sequence the
// console itself recommends turns the strongest refusal in that matrix into the
// allowance beside it, in one operation, with nothing in between.
//
// The trace is what tells the two paused domains apart.
func TestARestoreIsWithheldFromACopyThatServedLive(t *testing.T) {
	row := restorableRow()
	row.Role = store.RolePaused
	row.LastPromotedAt = "1756000000"
	row.LastPromotedAtUnix = 1756000000

	if row.CanRestore() {
		t.Fatal("a paused copy that was promoted and then shut down is offered a rollback that " +
			"would overwrite the data it served; the role cannot tell it from an ordinary paused replica")
	}
	if !row.ServedLive() {
		t.Error("ServedLive must read the record, since it is the only thing left saying this copy served")
	}
	// Withheld, not dead-ended: the operator has to be told what clears it,
	// and the command has to name this domain.
	if cmd := row.ReleaseCommand(); !strings.Contains(cmd, "-release-promotion") || !strings.Contains(cmd, row.VM) {
		t.Errorf("ReleaseCommand() = %q, want the command that releases THIS domain", cmd)
	}
	// And released, it goes back to being an ordinary paused replica. A
	// predicate that refused for ever would leave every pair that had once
	// failed over unable to use this page again.
	row.LastPromotedAt, row.LastPromotedAtUnix = "", 0
	if !row.CanRestore() {
		t.Error("after the record is released the rollback must be offered again; withholding it " +
			"permanently dead-ends every pair that has ever been failed over")
	}
}

// TestAResyncIsWithheldWhenThePEERServedLive: the destructive sync controls are
// offered on the SOURCE's row, so the row that can overwrite a copy which held
// production data is not the row the record is on. Reading the local trace here
// would protect nothing at all.
func TestAResyncIsWithheldWhenThePEERServedLive(t *testing.T) {
	src := FailoverRow{AgentID: "src", VM: "web01", IsSource: true,
		PeerHost: "hyper02p", PeerVM: "web01",
		PeerLastPromotedAt: "1756000000", PeerLastPromotedAtUnix: 1756000000}

	if src.CanReinit() {
		t.Error("a full resync is offered into a replica that has served live and not been released")
	}
	if src.CanForceClean() {
		t.Error("force-clean is offered over a replica that has served live -- it is the one control " +
			"that also overrides the role interlock, so it is the worst one to leave open")
	}
	if cmd := src.PeerReleaseCommand(); !strings.Contains(cmd, "-release-promotion") || !strings.Contains(cmd, "hyper02p") {
		t.Errorf("PeerReleaseCommand() = %q, want the command aimed at the PEER and naming its host", cmd)
	}
	// The local trace must not be what gates these: a source that was itself
	// promoted in an earlier cycle is the normal post-inversion state, and its
	// own history says nothing about the replica it writes to.
	src.PeerLastPromotedAt, src.PeerLastPromotedAtUnix = "", 0
	src.LastPromotedAt, src.LastPromotedAtUnix = "1600000000", 1600000000
	if !src.CanReinit() {
		t.Error("a source that was itself promoted in an earlier cycle cannot resync its replica; " +
			"the gate is reading the wrong end")
	}
}

// TestOnlyTargetIsWithheldFromTheRoleMenu: the escape hatch must not close.
// `target` is the one value that hands the domain back to the replication
// machinery, after which the next scheduled sync overwrites it unattended.
// `source` and `paused` destroy nothing and stay available -- and `source` is
// how an operator keeps this copy, which is the whole alternative the refusal
// points at.
func TestOnlyTargetIsWithheldFromTheRoleMenu(t *testing.T) {
	row := restorableRow()
	row.Role = store.RolePaused
	row.LastPromotedAt = "1756000000"

	if !row.CanSetRole() {
		t.Fatal("the role control is withheld entirely, which closes the only way back from this state")
	}
	if row.CanSetRoleTarget() {
		t.Error("`target` is offered on a copy that served live; the next scheduled sync would then " +
			"overwrite it with no further click")
	}
	row.LastPromotedAt = ""
	if !row.CanSetRoleTarget() {
		t.Error("`target` stays withheld after the record is released")
	}
}
