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
)

// The agent reports a result the moment it STARTS an operation, not only
// when it finishes: operationLedger.Begin stores a record whose state is
// "running", and every report carries it until the operation ends. A long
// promote or restore therefore sends "running" first and its outcome later.
//
// This UI used to treat any result as the outcome, which broke six things
// at once. All six are asserted here, because they share one cause and a
// fix that closed only the first would look complete.
func TestARunningResultIsProgressNotAnOutcome(t *testing.T) {
	s, prod, dr := twoAgents(t)

	// The old source's entry: a successful promotion must disable it, and
	// that follow-up is the step whose absence silently breaks the pair.
	if err := s.SetScheduleEntry(prod, ScheduleEntry{
		VM: "web01", IntervalSeconds: 900, Enabled: true, TargetHost: "dr01",
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}

	// The agent has started the promote and says so.
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, Kind: OpPromote, VM: "web01", State: OpStateRunning, StartedAtUnix: opNow.Unix()},
	}); err != nil {
		t.Fatalf("RecordOperationResults(running): %v", err)
	}

	// (1) It must stay published. Publication is what makes the agent
	// re-send, and the agent drops its own ledger record once the UI stops
	// -- so un-publishing here loses the outcome on both sides at once.
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 1 {
		t.Errorf("a running operation is published to %d agents' configs, want it still published; the agent stops re-sending and its outcome is lost", len(cfg.Operations))
	}

	// (2) The one-in-flight gate must stay shut. It exists so that two
	// promotions of one domain are not something an operator can create by
	// clicking twice, and an operation that is RUNNING is the case it most
	// needs to cover.
	if _, err := s.CreateOperation(dr, "", promoteOperation(), opNow); err == nil {
		t.Error("a second operation was accepted for a VM whose first one is still running")
	}

	// (3) The follow-up must NOT have been applied yet: nothing has
	// succeeded, so there is nothing to follow through.
	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(sched[prod]) == 1 && !sched[prod][0].Enabled {
		t.Error("the old source's entry was disabled on the strength of a 'running' report, before the promotion had succeeded")
	}

	// Now the outcome arrives.
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, Kind: OpPromote, VM: "web01", State: OpStateDone, FinishedAtUnix: opNow.Unix() + 30},
	}); err != nil {
		t.Fatalf("RecordOperationResults(done): %v", err)
	}

	// (4) The outcome must be recorded, not skipped as "already processed".
	ops, err := s.Operations()
	if err != nil {
		t.Fatal(err)
	}
	var got *OperationRecord
	for i := range ops {
		if ops[i].ID == rec.ID {
			got = &ops[i]
		}
	}
	if got == nil {
		t.Fatal("the operation vanished")
	}
	if got.Result == nil || got.Result.State != OpStateDone {
		t.Errorf("stored state is %v, want %q -- the outcome was discarded and the console would show this failover as running forever",
			func() string {
				if got.Result == nil {
					return "<nil>"
				}
				return got.Result.State
			}(), OpStateDone)
	}
	if !got.Succeeded() {
		t.Error("Succeeded() is false for an operation that reported done")
	}

	// (5) ...and the follow-up must now run, or the old source keeps
	// replicating into a domain that is live.
	sched, err = s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(sched[prod]) != 1 {
		t.Fatalf("the old source has %d entries, want its one entry kept", len(sched[prod]))
	}
	if sched[prod][0].Enabled {
		t.Error("the old source is still enabled after a successful promotion; it would keep syncing into the live copy")
	}

	// (6) Only now does publication end.
	cfg, _, err = s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("the operation is still published after reporting its outcome; the agent would never stop seeing it")
	}
}

// A state this build has never heard of counts as an OUTCOME, not as
// progress. The direction is deliberate: a state mistaken for progress
// keeps the operation published forever, keeps the agent re-sending it, and
// keeps the in-flight gate shut against every later operation for that VM.
// Closing out an unknown state costs a follow-up; refusing to close one
// wedges the VM.
func TestAnUnrecognisedStateIsTreatedAsAnOutcome(t *testing.T) {
	s, _, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, State: "something-a-newer-agent-invented"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("an unrecognised state left the operation published; that VM can never receive another operation")
	}
}

// Cancelling is for an operation that has not started. Once the agent has
// reported it running, the work is underway on a hypervisor and the UI
// cannot take it back -- so the refusal must stand, and must say the true
// reason rather than claiming the operation has already reported an
// outcome.
func TestCancelIsRefusedOnceTheAgentHasStarted(t *testing.T) {
	s, _, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, State: OpStateRunning},
	}); err != nil {
		t.Fatal(err)
	}

	err = s.CancelOperation(rec.ID, "alice", opNow)
	if err == nil {
		t.Fatal("a running operation was cancelled; the UI cannot recall work already underway on a hypervisor")
	}
	if !strings.Contains(err.Error(), "running") && !strings.Contains(err.Error(), "started") {
		t.Errorf("the refusal does not say the operation is already underway: %v", err)
	}
}

// ...but an operation nobody has picked up yet is still cancellable.
func TestCancelStillWorksBeforeTheAgentReports(t *testing.T) {
	s, _, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelOperation(rec.ID, "alice", opNow); err != nil {
		t.Fatalf("CancelOperation on an unstarted operation: %v", err)
	}
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("a cancelled operation is still published")
	}
}

// THE UPGRADE HAZARD. A build that recorded "running" as an outcome left
// records carrying a non-terminal result AND FollowUpDone, having told the
// agent its result was received -- so the agent dropped its ledger entry.
//
// Re-reading such a record with the fixed Pending() would make it pending
// again, re-publish it to an agent with no memory of it, and inside the
// fifteen minutes OperationTTL allows, run a forced promote a second time
// because the console was upgraded. This pins that it cannot happen.
func TestARecordStuckByTheOldBuildIsNotReDelivered(t *testing.T) {
	s, _, dr := twoAgents(t)
	rec, err := s.CreateOperation(dr, "", promoteOperation(), opNow)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what the old build wrote: the progress report stored as the
	// result, with the follow-up marked done.
	ops, err := s.loadOperations()
	if err != nil {
		t.Fatal(err)
	}
	stuck := ops[rec.ID]
	stuck.Result = &OperationResult{ID: rec.ID, Kind: OpPromote, VM: "web01", State: OpStateRunning}
	stuck.FollowUpDone = true
	ops[rec.ID] = stuck
	if err := writeJSONAtomic(s.operationsPath(), ops, 0o600); err != nil {
		t.Fatal(err)
	}

	// The upgraded build reads it back.
	cfg, _, err := s.AgentConfigFor(dr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Operations) != 0 {
		t.Error("a record left at 'running' by the old build was re-published; the agent has forgotten it and would execute the operation again")
	}

	// ...and it is reported honestly rather than left looking live.
	got, err := s.Operations()
	if err != nil {
		t.Fatal(err)
	}
	var found *OperationRecord
	for i := range got {
		if got[i].ID == rec.ID {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatal("the operation vanished")
	}
	if found.Result == nil || found.Result.State != OpStateUnknown {
		t.Errorf("stuck record reads as %+v, want state %q so that somebody is told to go and look", found.Result, OpStateUnknown)
	}
	if found.Result != nil && found.Result.Error == "" {
		t.Error("the healed record carries no explanation, so an operator sees 'unknown' with no reason")
	}
	if found.Succeeded() {
		t.Error("a record whose outcome was lost reports as succeeded")
	}
}

// The healing must not touch a record this build wrote. A legitimately
// running operation has no FollowUpDone, and must stay running.
func TestHealingLeavesAGenuinelyRunningOperationAlone(t *testing.T) {
	rec := OperationRecord{
		Result: &OperationResult{State: OpStateRunning},
		// FollowUpDone deliberately false: this is what the fixed build
		// writes for a progress report.
	}
	if _, changed := healStuckProgressRecord(rec); changed {
		t.Error("a genuinely running operation was rewritten as unknown; a failover in progress would be reported as lost")
	}
	if !rec.Pending() {
		t.Error("a genuinely running operation is not pending, so the agent would stop being told about it")
	}

	// And a terminal record is left alone whatever its flag says.
	for _, st := range []string{OpStateDone, OpStateFailed, OpStateRefused, OpStateExpired, OpStateUnknown} {
		done := OperationRecord{Result: &OperationResult{State: st}, FollowUpDone: true}
		if _, changed := healStuckProgressRecord(done); changed {
			t.Errorf("a terminal %q record was rewritten", st)
		}
	}
}

// The audit entry records the OUTCOME of what somebody asked for, and it
// closes once. Closing it on a progress report spent that one write on the
// word "running", so the permanent record of a failover said neither
// whether it worked nor when it ended.
func TestTheAuditEntryIsClosedByTheOutcomeNotTheProgress(t *testing.T) {
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
		{ID: rec.ID, State: OpStateRunning},
	}); err != nil {
		t.Fatal(err)
	}

	entryFor := func(id string) AuditEntry {
		t.Helper()
		entries, err := s.Audit()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.ID == id {
				return e
			}
		}
		t.Fatalf("audit entry %s vanished", id)
		return AuditEntry{}
	}

	if got := entryFor(auditID); got.Outcome != "" || got.DoneUnix != 0 {
		t.Errorf("the audit entry closed on a progress report (outcome=%q done=%d); the permanent record of this failover would say 'running' and never be corrected",
			got.Outcome, got.DoneUnix)
	}

	// The real outcome closes it.
	if err := s.RecordOperationResults(dr, []OperationResult{
		{ID: rec.ID, State: OpStateDone},
	}); err != nil {
		t.Fatal(err)
	}
	if got := entryFor(auditID); got.Outcome != OpStateDone {
		t.Errorf("audit outcome = %q, want %q", got.Outcome, OpStateDone)
	}
}
