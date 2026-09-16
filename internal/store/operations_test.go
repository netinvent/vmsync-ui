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

package store

import (
	"strings"
	"testing"
	"time"
)

var opNow = time.Unix(1_800_000_000, 0)

// twoAgents enrols a source and a target host, returning their IDs.
func twoAgents(t *testing.T) (*Store, string, string) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	enrol := func(host string) string {
		t.Helper()
		tok, err := s.CreateEnrolmentToken(host, "op", time.Hour)
		if err != nil {
			t.Fatalf("token for %s: %v", host, err)
		}
		a, _, err := s.Enrol(tok, host, "0.40")
		if err != nil {
			t.Fatalf("enrol %s: %v", host, err)
		}
		return a.ID
	}
	return s, enrol("prod01"), enrol("dr01")
}

func promoteOperation() Operation {
	return Operation{
		Kind: OpPromote, VM: "web01",
		PeerHost: "prod01", PeerVM: "web01",
		Mode: "forced", StartVM: true, CreatedBy: "alice",
	}
}

// TestOperationPublishedAndAcknowledged walks the whole lifecycle, and the
// two ends of it are what matter: publishing must move the ETag or the
// agent never hears about it, and a result must stop publication or the
// agent is told to do it forever.
func TestOperationPublishedAndAcknowledged(t *testing.T) {
	s, _, dr := twoAgents(t)

	_, before, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}

	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("no operation id was generated")
	}
	if rec.NotAfterUnix <= opNow.Unix() {
		t.Error("no expiry was set; the operation would stay armed forever")
	}

	cfg, afterCreate, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if afterCreate == before {
		t.Fatal("the ETag did not move, so a polling agent would keep getting 304 and never see the operation")
	}
	if len(cfg.Operations) != 1 || cfg.Operations[0].ID != rec.ID {
		t.Fatalf("config carries %+v, want the new operation", cfg.Operations)
	}

	// It must go to THAT agent and no other.
	other, _, err := s.AgentConfigFor("some-other-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Operations) != 0 {
		t.Error("the operation leaked into another agent's config")
	}

	if err := s.RecordOperationResults(dr, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatalf("RecordOperationResults: %v", err)
	}
	cfg, afterResult, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("the operation is still published after reporting; the agent would never stop seeing it")
	}
	if afterResult == afterCreate {
		t.Error("the ETag did not move when the operation was acknowledged")
	}
}

// TestRecordOperationResultsIsIdempotent is what makes the crash-safe
// ordering work: consequences are applied before the result is recorded, so
// the step must be safe to repeat after a crash in between.
func TestRecordOperationResultsIsIdempotent(t *testing.T) {
	s, prod, dr := twoAgents(t)

	// The old source has a schedule entry that a successful promotion must
	// disable.
	if err := s.SetScheduleEntry(prod, ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01"}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}

	res := []OperationResult{{ID: rec.ID, State: OpStateDone}}
	for i := 0; i < 3; i++ {
		if err := s.RecordOperationResults(dr, res); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(sched[prod]) != 1 {
		t.Fatalf("the old source has %d entries, want its one entry kept (disabled, not deleted)", len(sched[prod]))
	}
	if sched[prod][0].Enabled {
		t.Error("the old source is still scheduled to sync into a domain that is now live")
	}
	if sched[prod][0].IntervalSeconds != 900 {
		t.Error("disabling the entry lost the settings an operator tuned")
	}
}

// TestInvertMigratesTheScheduleInOneWrite: the entry lives under the OLD
// source's agent, which after an inversion is no longer the source. Leaving
// it there means the pair silently stops replicating, with nothing
// reporting an error -- an absent schedule entry looks exactly like a VM
// nobody asked to replicate.
func TestInvertMigratesTheScheduleInOneWrite(t *testing.T) {
	s, prod, dr := twoAgents(t)

	if err := s.SetScheduleEntry(prod, ScheduleEntry{
		VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01",
		Profile: SyncProfile{Compress: "zstd", CompressLevel: "5", Verify: "full"},
	}); err != nil {
		t.Fatal(err)
	}

	// An inversion runs on the OLD source's agent, naming the promoted peer.
	op := Operation{Kind: OpInvert, VM: "web01", PeerHost: "dr01", PeerVM: "web01", CreatedBy: "alice"}
	rec, err := s.CreateOperation(prod, "", op, opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatal(err)
	}

	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(sched[prod]) != 0 {
		t.Errorf("the entry is still under the old source (%d), which is now the target", len(sched[prod]))
	}
	if len(sched[dr]) != 1 {
		t.Fatalf("the new source has %d entries, want the migrated one", len(sched[dr]))
	}
	got := sched[dr][0]
	if got.TargetHost != "prod01" {
		t.Errorf("TargetHost = %q, want prod01 -- replication now runs the other way", got.TargetHost)
	}
	if got.Profile.Compress != "zstd" || got.Profile.Verify != "full" {
		t.Errorf("the migrated entry lost its profile: %+v", got.Profile)
	}
	// The first sync in the reversed direction has no checkpoint chain and
	// must be a full reinit, which the schedule cannot express yet. Arriving
	// enabled would schedule a run that fails every interval.
	if got.Enabled {
		t.Error("the migrated entry is enabled; its first run would fail every interval until someone did a full sync by hand")
	}
}

func TestCancelOperation(t *testing.T) {
	s, _, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CancelOperation(rec.ID, "bob", opNow); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("a cancelled operation is still published")
	}

	// Cancelling after the fact must fail loudly rather than pretend: the
	// work has happened on a hypervisor and no UI state undoes it.
	rec2, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(dr, []OperationResult{{ID: rec2.ID, State: OpStateDone}}); err != nil {
		t.Fatal(err)
	}
	err = s.CancelOperation(rec2.ID, "bob", opNow)
	if err == nil {
		t.Fatal("cancelled an operation that had already reported")
	}
	if !strings.Contains(err.Error(), "already reported") {
		t.Errorf("error = %q, want it to say the operation already ran", err)
	}
}

// TestOneOperationPerVM: two promotions of one domain, or a promote racing
// an invert, is not a state an operator should be able to create by
// clicking twice on a slow page.
func TestOneOperationPerVM(t *testing.T) {
	s, _, dr := twoAgents(t)
	if _, err := s.CreateOperation(dr, "", promoteOperation(), opNow); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err == nil {
		t.Fatal("accepted a second in-flight operation for the same VM")
	}
	if !strings.Contains(err.Error(), "already has an operation in flight") {
		t.Errorf("error = %q", err)
	}
}

// TestResultsOnlyAcceptedFromTheAgentTheyWereIssuedTo: otherwise one host's
// credential could close out another host's failover.
func TestResultsOnlyAcceptedFromTheAgentTheyWereIssuedTo(t *testing.T) {
	s, prod, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatalf("RecordOperationResults: %v", err)
	}
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 1 {
		t.Error("another agent's report closed out this operation")
	}
}

// TestExpiredOperationIsStillPublished: expiry is enforced by the AGENT,
// which refuses and reports. Withdrawing it here instead would leave the
// audit entry open forever with nothing ever saying what became of it.
func TestExpiredOperationIsStillPublished(t *testing.T) {
	s, _, dr := twoAgents(t)
	op := promoteOperation()
	op.NotAfterUnix = opNow.Unix() - 1
	rec, err := s.CreateOperation(dr, "", op, opNow)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Expired(opNow) {
		t.Error("Expired() does not report a past deadline")
	}
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 1 {
		t.Error("an expired operation was withdrawn; the agent would never report a refusal and the audit entry would hang open")
	}
}

// TestOperationCompletesItsAuditEntry: the audit records intent before the
// action, and this is what writes the outcome back against it.
func TestOperationCompletesItsAuditEntry(t *testing.T) {
	s, _, dr := twoAgents(t)
	auditID, err := s.AppendAudit("alice", "promote", "web01", "forced")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateOperation(dr, auditID, promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, State: OpStateFailed, Error: "exit status 1"},
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Audit()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.ID == auditID {
			found = true
			if e.Outcome == "" {
				t.Error("the audit entry has no outcome; a failed failover would read as still in progress")
			}
			if !strings.Contains(e.Outcome, "exit status 1") {
				t.Errorf("outcome = %q, want the failure reason", e.Outcome)
			}
		}
	}
	if !found {
		t.Fatal("the audit entry disappeared")
	}
}

// TestUnknownResultIsNotAnError: the agent re-sends until acknowledged, so a
// result for something already cleaned up here is an acknowledgement
// arriving late, not a fault.
func TestUnknownResultIsNotAnError(t *testing.T) {
	s, _, dr := twoAgents(t)
	if err := s.RecordOperationResults(dr, []OperationResult{{ID: "never-existed", State: OpStateDone}}); err != nil {
		t.Errorf("a result for an unknown operation was treated as an error: %v", err)
	}
}

// An inversion must re-aim target_disk_path at where the NEW target's disks
// actually are.
//
// That value describes where THIS pair's replicas went, so after an
// inversion it points at the new SOURCE's own disks. Carried across
// unchanged -- which is what the wholesale entry copy used to do -- the
// first reversed sync writes to the wrong directory on the wrong host:
// either failing because it does not exist there, or, where it does,
// creating the replica there and redefining the domain to match, silently
// orphaning the original disk.
func TestInvertReAimsTheTargetDiskPath(t *testing.T) {
	s, prod, dr := twoAgents(t)

	// The asymmetric layout this exists for: production disks in the
	// distribution's own directory, replicas in a dedicated one.
	if err := s.SaveReport(prod, Report{
		Hostname: "prod01",
		Domains: []ReportDomain{{
			Name:  "web01",
			Disks: []ReportDisk{{Path: "/var/lib/libvirt/images/web01.qcow2"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScheduleEntry(prod, ScheduleEntry{
		VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01",
		Profile: SyncProfile{Verify: "full", TargetDiskPath: "/data/replicas"},
	}); err != nil {
		t.Fatal(err)
	}

	op := Operation{Kind: OpInvert, VM: "web01", PeerHost: "dr01", PeerVM: "web01", CreatedBy: "alice"}
	rec, err := s.CreateOperation(prod, "", op, opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatal(err)
	}

	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(sched[dr]) != 1 {
		t.Fatalf("the new source has %d entries, want the migrated one", len(sched[dr]))
	}
	got := sched[dr][0].Profile.TargetDiskPath
	if got == "/data/replicas" {
		t.Fatal("target_disk_path was carried across unchanged -- the reversed sync would aim at the new SOURCE's own replica directory, on the old source's host")
	}
	if got != "/var/lib/libvirt/images" {
		t.Errorf("target_disk_path = %q, want /var/lib/libvirt/images -- where the new target's disks already live", got)
	}
	// Everything else about the profile must survive.
	if sched[dr][0].Profile.Verify != "full" {
		t.Error("re-aiming the disk path disturbed the rest of the profile")
	}
}

// Left alone when it cannot be reduced to one directory: target_disk_path is
// a single directory for every disk, so a domain whose disks span several
// cannot be expressed by it in either direction, and picking one would be
// worse than leaving a value an operator can see.
func TestInvertLeavesTheDiskPathAloneWhenItCannotBeDetermined(t *testing.T) {
	for _, tc := range []struct {
		name  string
		disks []ReportDisk
	}{
		{"disks in two directories", []ReportDisk{
			{Path: "/var/lib/libvirt/images/web01.qcow2"},
			{Path: "/srv/extra/web01-data.qcow2"},
		}},
		{"no disks reported at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, prod, dr := twoAgents(t)
			if err := s.SaveReport(prod, Report{
				Hostname: "prod01",
				Domains:  []ReportDomain{{Name: "web01", Disks: tc.disks}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetScheduleEntry(prod, ScheduleEntry{
				VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01",
				Profile: SyncProfile{TargetDiskPath: "/data/replicas"},
			}); err != nil {
				t.Fatal(err)
			}
			op := Operation{Kind: OpInvert, VM: "web01", PeerHost: "dr01", PeerVM: "web01"}
			rec, err := s.CreateOperation(prod, "", op, opNow)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
				t.Fatal(err)
			}
			sched, err := s.Schedules()
			if err != nil {
				t.Fatal(err)
			}
			if len(sched[dr]) != 1 {
				t.Fatalf("got %d entries", len(sched[dr]))
			}
			if got := sched[dr][0].Profile.TargetDiskPath; got != "/data/replicas" {
				t.Errorf("target_disk_path = %q, want it left at /data/replicas rather than guessed at", got)
			}
		})
	}
}

// The symmetric case, which is the common one: no target_disk_path set at
// all means "the same path as the source", which survives an inversion by
// itself. Re-aiming must not invent a value where none was wanted.
func TestInvertOnASymmetricPairSetsThePathItFinds(t *testing.T) {
	s, prod, dr := twoAgents(t)
	if err := s.SaveReport(prod, Report{
		Hostname: "prod01",
		Domains: []ReportDomain{{
			Name:  "web01",
			Disks: []ReportDisk{{Path: "/var/lib/libvirt/images/web01.qcow2"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScheduleEntry(prod, ScheduleEntry{
		VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01",
	}); err != nil {
		t.Fatal(err)
	}
	op := Operation{Kind: OpInvert, VM: "web01", PeerHost: "dr01", PeerVM: "web01"}
	rec, err := s.CreateOperation(prod, "", op, opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatal(err)
	}
	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	// Naming the directory explicitly is equivalent to leaving it empty here
	// -- both put the replica at the same path -- and being explicit is what
	// keeps it correct if the pair is ever inverted again.
	if got := sched[dr][0].Profile.TargetDiskPath; got != "/var/lib/libvirt/images" {
		t.Errorf("target_disk_path = %q, want the old source's own directory", got)
	}
}

// A restore rolls the replica's disks back and pauses replication into it,
// because the next sync from the same source would put back exactly what was
// rolled away from. The role stops it at the far end; disabling the source's
// schedule entry stops the source trying at all.
//
// Disabled rather than deleted, like a promotion's: resuming means re-enabling
// this same entry, and deleting it would lose the profile somebody tuned.
func TestRestoreDisablesTheSourcesSchedule(t *testing.T) {
	s, prod, dr := twoAgents(t)
	if err := s.SetScheduleEntry(prod, ScheduleEntry{VM: "web01", IntervalSeconds: 3600, Enabled: true, Profile: SyncProfile{Retention: "24,3h"}}); err != nil {
		t.Fatalf("SetScheduleEntry: %v", err)
	}

	// Issued to the REPLICA's agent, naming the source as its peer.
	rec, err := s.CreateOperation(dr, "", Operation{
		Kind: OpRestore, VM: "web01", PeerHost: "prod01", PeerVM: "web01",
		Tag: "1756041600-vmsync-cpt-000042", CreatedBy: "alice",
	}, opNow)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if err := s.RecordOperationResults(dr, []OperationResult{{ID: rec.ID, State: OpStateDone}}); err != nil {
		t.Fatalf("RecordOperationResults: %v", err)
	}

	sched, err := s.Schedules()
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	entry := sched[prod][0]
	if entry.Enabled {
		t.Error("the source is still scheduled to sync into a replica that was just rolled back; the next run would put back exactly what the operator rolled away from")
	}
	if entry.Profile.Retention != "24,3h" {
		t.Error("the entry was rebuilt rather than disabled, losing the profile somebody tuned")
	}
}

// The other half of going back to replicating: a reinit rebuilds the replica
// from scratch, which is the only way back, because a restore leaves metadata
// describing an older checkpoint and an incremental is refused by design.
//
// Re-enabled on SUCCESS only. An entry switched back on by a reinit that then
// failed would resume scheduled syncs against a replica still in the state the
// operator was trying to leave.
func TestReinitReenablesTheScheduleOnlyWhenItSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		wantAfter bool
	}{
		{"succeeded", OpStateDone, true},
		{"failed", OpStateFailed, false},
		{"refused", OpStateRefused, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, prod, _ := twoAgents(t)
			if err := s.SetScheduleEntry(prod, ScheduleEntry{VM: "web01", IntervalSeconds: 3600, Enabled: false}); err != nil {
				t.Fatalf("SetScheduleEntry: %v", err)
			}
			// Issued to the SOURCE's agent: a reinit is a sync.
			rec, err := s.CreateOperation(prod, "", Operation{
				Kind: OpReinit, VM: "web01", CreatedBy: "alice",
			}, opNow)
			if err != nil {
				t.Fatalf("CreateOperation: %v", err)
			}
			if err := s.RecordOperationResults(prod, []OperationResult{{ID: rec.ID, State: tc.state}}); err != nil {
				t.Fatalf("RecordOperationResults: %v", err)
			}
			sched, err := s.Schedules()
			if err != nil {
				t.Fatalf("Schedules: %v", err)
			}
			if got := sched[prod][0].Enabled; got != tc.wantAfter {
				t.Errorf("after a %s reinit the schedule is enabled=%v, want %v", tc.name, got, tc.wantAfter)
			}
		})
	}
}
