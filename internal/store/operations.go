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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Operation mirrors the agent's type field for field. Like SyncProfile it is
// a wire contract between two separately-versioned programs, so it is
// written out here rather than shared, and changes only when the protocol
// does.
type Operation struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	VM       string `json:"vm"`
	PeerHost string `json:"peer_host,omitempty"`
	PeerVM   string `json:"peer_vm,omitempty"`
	Mode     string `json:"mode,omitempty"`
	StartVM  bool   `json:"start_vm,omitempty"`
	Force    bool   `json:"force,omitempty"`
	// ArmFence asks the promotion to authorise shutting the displaced source
	// down, so one VM does not end up serving in two places.
	//
	// Opt-in from here to the far end. A DR drill is a promotion too, and a
	// drill that stopped production would be a worse failure than the split
	// brain it was rehearsing for -- so nothing infers this, and the default
	// of every path that reaches it is off.
	//
	// Note what does NOT travel: which source to fence. The agent asks
	// vmsync to resolve that from the promoted domain's own replica_source,
	// so this flag can request a fence but can never choose its victim.
	ArmFence bool `json:"arm_fence,omitempty"`
	// ShutdownTimeoutSec is how long a clean guest shutdown may take, for
	// the operations that perform one.
	//
	// Resolved when the operation is CREATED, from the VM's own setting or
	// the estate default, and carried here rather than looked up by the
	// agent. An operation is a record of a decision, and a decision that
	// silently meant 300 seconds in March and 900 in April -- because
	// somebody edited a setting in between -- is not one anybody can audit.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`
	// Tag names the restore point a restore operation puts back.
	//
	// The one operation parameter that cannot be checked against the target's
	// own libvirt metadata the way PeerHost is, because a restore point is a
	// directory rather than anything a domain records. It comes from that
	// agent's own report, and the agent re-checks it against the filesystem
	// before acting -- so a tag that has since been pruned is refused rather
	// than acted on.
	Tag string `json:"tag,omitempty"`

	// ActionID is the correlation id the agent passes to the engine as
	// -action-id, and which the engine stamps into every intent and outcome
	// record it journals beside the disks it touched.
	//
	// It is this UI's own audit entry id, deliberately, rather than a second
	// identifier minted here. That is what closes the loop across three
	// programs that each keep their own record and share no storage: the
	// audit entry says who asked and why, the operation says what was
	// published to which agent, and the journal on the hypervisor says what
	// the engine then did to the disks. Without one id running through all
	// three, an operator holding a half-written replica has to correlate
	// them by timestamp, which is exactly the evidence that goes ambiguous
	// when a run was retried.
	//
	// Optional and omitempty because it must stay so in both directions. An
	// operation issued before this field existed carries none, and an agent
	// too old to know it ignores it -- the config an agent polls is decoded
	// leniently, unlike the reports it sends, which is what makes "UI first"
	// a safe upgrade order rather than merely a preferred one.
	ActionID string `json:"action_id,omitempty"`

	CreatedAtUnix int64  `json:"created_at_unix"`
	CreatedBy     string `json:"created_by,omitempty"`
	NotAfterUnix  int64  `json:"not_after_unix,omitempty"`
}

// OperationResult mirrors the agent's outcome type.
type OperationResult struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	VM             string `json:"vm"`
	State          string `json:"state"`
	StartedAtUnix  int64  `json:"started_at_unix,omitempty"`
	FinishedAtUnix int64  `json:"finished_at_unix,omitempty"`
	ExitCode       int    `json:"exit_code,omitempty"`
	Error          string `json:"error,omitempty"`
	LogTail        string `json:"log_tail,omitempty"`
}

// Operation kinds and result states, mirroring the agent's own constants.
const (
	OpPromote  = "promote"
	OpInvert   = "invert"
	OpShutdown = "shutdown-domain"
	OpSetRole  = "set-role"
	// OpRestore rolls a replica back to one of its restore points, in place,
	// and pauses replication into it. Runs on the TARGET's agent.
	OpRestore = "restore"
	// OpReinit is a one-shot full resync. Runs on the SOURCE's agent, unlike
	// every other kind here except invert, because it is a sync and needs
	// that pair's whole transport configuration.
	//
	// It is the second half of going back to replicating after a restore:
	// the restore leaves the replica paused with metadata describing an
	// older checkpoint, so an ordinary incremental is refused by design. Set
	// the role back to target, then reinit.
	OpReinit = "reinit"
	// OpForceClean is a reinit for a target an ordinary reinit cannot get
	// past: it undefines the target domain, overrides the promoted and
	// paused role interlocks, and clears a shut-down source's checkpoint
	// chain including its bitmaps. Runs on the SOURCE's agent, like reinit.
	//
	// Its own kind rather than a flag on reinit so the audit trail says
	// which one ran -- they destroy different amounts of the target.
	OpForceClean = "force-clean"

	// OpStateRunning is the one state that is PROGRESS rather than an
	// outcome, and the only one of these the agent sends while it still
	// holds the operation.
	//
	// It was the one agent state missing from this list, which is how a
	// report meaning "I have started" came to be recorded as "I have
	// finished". The agent writes it in operationLedger.Begin -- before the
	// work, deliberately, so a crash cannot lose the fact that a failover
	// was attempted -- and attaches it to every report until the operation
	// ends. A promote or a restore takes long enough that this is the
	// ordinary case, not a rare one.
	OpStateRunning = "running"

	OpStateDone    = "done"
	OpStateFailed  = "failed"
	OpStateExpired = "expired"
	OpStateRefused = "refused"
	OpStateUnknown = "unknown"
)

// Terminal reports whether this result is an OUTCOME rather than progress.
//
// Everything except OpStateRunning counts as an outcome, INCLUDING a state
// this build has never heard of, and that direction is deliberate rather
// than lazy. A state mistaken for progress keeps the operation published
// forever, keeps the agent re-sending it, and keeps the one-in-flight gate
// shut against every later operation for that VM. A state mistaken for an
// outcome costs one follow-up. Wedging a VM is the worse failure, so the
// unknown case resolves towards closing the operation out -- which is also
// exactly what this build did with every state before "running" was
// understood here.
//
// OpStateUnknown is an outcome too, and not a contradiction: it is the
// agent's considered report that it died mid-operation and cannot say what
// happened, which is a final answer needing a person, not a progress
// update.
func (r OperationResult) Terminal() bool {
	return r.State != OpStateRunning
}

// Replication roles, as vmsync writes them into a domain's own metadata and
// the agent reports them back.
//
// Named here for the same reason the kinds above are: they arrive from a
// separately-versioned program, they are compared in several places, and a
// literal typed slightly wrong compiles perfectly and then quietly fails to
// match -- which for a role means an unresolved failover that no page shows.
//
// An empty role is a real and common value: it is what every domain
// predating the feature carries, and vmsync treats it as an ordinary
// replication target rather than as an error.
const (
	RoleSource   = "source"
	RoleTarget   = "target"
	RolePromoted = "promoted"
	RolePaused   = "paused"
	// RoleFenced is a domain an automatic fence stopped after a peer was
	// promoted over it. Distinct from RolePaused, which means a person chose
	// to suspend replication; nobody chose this one.
	RoleFenced = "fenced"
)

// OperationTTL is how long an operation stays executable.
//
// Minutes, not hours, and it is the difference between an instruction and a
// standing order. An agent that was unreachable when a forced promote was
// issued would otherwise execute it whenever it eventually came back --
// a first delivery, so no replay guard covers it -- long after the incident
// that motivated it was over.
const OperationTTL = 15 * time.Minute

// OperationRecord is an operation plus everything the UI knows about it that
// the agent does not need.
type OperationRecord struct {
	Operation
	// AgentID is who it is published to. Operations are per-agent: the one
	// that runs a promotion is the agent on the host holding the replica.
	AgentID string `json:"agent_id"`
	// AuditID links to the audit entry recording who asked for this, so the
	// outcome can be written back against it.
	AuditID string `json:"audit_id,omitempty"`
	// Result is nil until the agent reports one.
	Result *OperationResult `json:"result,omitempty"`
	// FollowUpDone marks the UI-side consequences (migrating a schedule,
	// disabling one) as applied. Separate from Result so that processing a
	// report is idempotent: a crash between applying them and recording the
	// result must not apply them twice.
	FollowUpDone bool `json:"follow_up_done,omitempty"`

	CancelledAtUnix int64  `json:"cancelled_at_unix,omitempty"`
	CancelledBy     string `json:"cancelled_by,omitempty"`
}

// Pending reports whether this operation should still be published to its
// agent.
//
// A TERMINAL result ends publication -- that is what acknowledges it to the
// agent, which drops its own ledger record in turn. A non-terminal one does
// not, and the difference is load-bearing: an agent reports "running" as
// soon as it starts, and un-publishing on that told the agent its outcome
// had been received before it had one. Both sides then discarded it -- the
// UI by skipping the later report, the agent by forgetting the operation it
// was no longer being told about.
//
// Cancelling ends publication too. Expiry deliberately does NOT: the agent
// must still see it, refuse it and report that refusal, so the audit entry
// closes with a reason instead of hanging as "pending" forever.
func (r OperationRecord) Pending() bool {
	if r.CancelledAtUnix != 0 {
		return false
	}
	return r.Result == nil || !r.Result.Terminal()
}

// Started reports whether the agent has picked this operation up.
//
// Distinct from !Pending(): an operation reported as running is both
// started and still pending, and the two questions have different answers
// for it. Cancelling asks this one.
func (r OperationRecord) Started() bool {
	return r.Result != nil
}

// Expired reports whether the deadline has passed, for display.
func (r OperationRecord) Expired(now time.Time) bool {
	return r.NotAfterUnix > 0 && now.Unix() > r.NotAfterUnix
}

// Succeeded reports whether the agent carried the operation out.
func (r OperationRecord) Succeeded() bool {
	return r.Result != nil && r.Result.State == OpStateDone
}

func (s *Store) operationsPath() string { return s.path("operations.json") }

func (s *Store) loadOperations() (map[string]OperationRecord, error) {
	var ops map[string]OperationRecord
	if _, err := readJSON(s.operationsPath(), &ops); err != nil {
		return nil, err
	}
	if ops == nil {
		ops = map[string]OperationRecord{}
	}
	for id, rec := range ops {
		if normalised, changed := healStuckProgressRecord(rec); changed {
			ops[id] = normalised
		}
	}
	return ops, nil
}

// healStuckProgressRecord repairs a record left inconsistent by a build that
// treated a progress report as an outcome.
//
// THE REASON THIS IS NOT COSMETIC. A non-terminal result makes an operation
// Pending() again, which re-publishes it to its agent -- and the agent,
// having been told by the old build that its result was received, dropped
// its own ledger entry. A record left at "running" by the old build would
// therefore be delivered to an agent with no memory of it, inside the
// fifteen minutes OperationTTL allows, and a forced promote would run a
// second time BECAUSE the UI was upgraded.
//
// `running` together with FollowUpDone is a combination this build cannot
// write: FollowUpDone is set only for a terminal result. So it identifies
// old data exactly, with no version stamp to keep in step.
//
// Normalised to OpStateUnknown because that is the honest answer and it is
// already this system's word for it -- the outcome was discarded when it
// arrived, so what happened on that hypervisor is now something only an
// inspection establishes. Terminal, so the operation stays acknowledged and
// is never re-delivered; visible, so somebody is told rather than finding a
// promotion silently stuck at "running".
//
// In memory only, deliberately. loadOperations is on every read path, and a
// read that writes would need the write lock and could fail a page load
// over a repair. The normalisation is idempotent, so it simply re-applies
// until some ordinary write persists it.
func healStuckProgressRecord(rec OperationRecord) (OperationRecord, bool) {
	if rec.Result == nil || rec.Result.Terminal() || !rec.FollowUpDone {
		return rec, false
	}
	healed := *rec.Result
	healed.State = OpStateUnknown
	if healed.Error == "" {
		healed.Error = "this operation was recorded as started but its outcome was never kept; what it did is unknown and needs checking on the host"
	}
	rec.Result = &healed
	return rec, true
}

// Operations returns every record, newest first.
func (s *Store) Operations() ([]OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.loadOperations()
	if err != nil {
		return nil, err
	}
	out := make([]OperationRecord, 0, len(ops))
	for _, r := range ops {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAtUnix > out[j].CreatedAtUnix })
	return out, nil
}

// CreateOperation records a new operation for one agent.
//
// The ID is generated here rather than accepted from a caller: it is the
// idempotency key the agent's whole replay guard hangs off, so it must be
// unique by construction and not by anybody remembering to make it so.
func (s *Store) CreateOperation(agentID, auditID string, op Operation, now time.Time) (OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if agentID == "" || op.VM == "" || op.Kind == "" {
		return OperationRecord{}, fmt.Errorf("an operation needs an agent, a vm and a kind")
	}

	// One in-flight operation per VM. Two promotions of the same domain, or
	// a promote racing an invert, is not something an operator should be
	// able to create by clicking twice on a slow page.
	ops, err := s.loadOperations()
	if err != nil {
		return OperationRecord{}, err
	}
	for _, r := range ops {
		if r.Pending() && strings.EqualFold(r.VM, op.VM) {
			return OperationRecord{}, fmt.Errorf("%s already has an operation in flight (%s, issued by %s); wait for it to report or cancel it", op.VM, r.Kind, r.CreatedBy)
		}
	}

	id, err := newOperationID()
	if err != nil {
		return OperationRecord{}, err
	}
	op.ID = id
	op.CreatedAtUnix = now.Unix()
	if op.NotAfterUnix == 0 {
		op.NotAfterUnix = now.Add(OperationTTL).Unix()
	}
	// The correlation id travels as the AUDIT entry's id, set here rather
	// than by each caller so no path can publish an operation that the
	// journal on the hypervisor cannot be tied back to. A caller that
	// already chose one is left alone; one issued with no audit entry at
	// all -- which is every test fixture and nothing in the handlers --
	// simply carries none, because an empty id is honest and a fabricated
	// one would join a journal record to an audit entry that does not exist.
	if op.ActionID == "" {
		op.ActionID = auditID
	}

	rec := OperationRecord{Operation: op, AgentID: agentID, AuditID: auditID}
	ops[id] = rec
	if err := writeJSONAtomic(s.operationsPath(), ops, 0o600); err != nil {
		return OperationRecord{}, err
	}
	return rec, nil
}

// CancelOperation withdraws an operation that has not reported yet.
//
// The counterpart to the expiry: a deadline handles an agent that never
// came back, and this handles an operator who changed their mind while it
// still might. Without one of them an armed promotion has no off switch.
func (s *Store) CancelOperation(id, by string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.loadOperations()
	if err != nil {
		return err
	}
	rec, ok := ops[id]
	if !ok {
		return fmt.Errorf("no such operation %s", id)
	}
	// Started, not !Pending(). A cancel withdraws an instruction the agent
	// has not acted on yet; once it has reported anything at all -- an
	// outcome, or "running" -- the work is underway or over on a hypervisor
	// and no amount of UI state changes that.
	//
	// The distinction matters now that a running operation stays pending.
	// Reading Pending() here would make a cancel appear to succeed against
	// a promotion already in progress, and the record would then carry both
	// a cancellation and that promotion's outcome.
	if rec.Started() {
		if rec.Result.Terminal() {
			return fmt.Errorf("operation %s has already reported and cannot be cancelled", id)
		}
		return fmt.Errorf("operation %s is already running on %s and cannot be cancelled; it will report its outcome when it finishes", id, rec.VM)
	}
	if !rec.Pending() {
		return fmt.Errorf("operation %s is no longer pending and cannot be cancelled", id)
	}
	rec.CancelledAtUnix = now.Unix()
	rec.CancelledBy = by
	ops[id] = rec
	return writeJSONAtomic(s.operationsPath(), ops, 0o600)
}

// PendingOperationsFor returns what an agent should currently execute.
//
// Callers already holding the lock use this; AgentConfigFor does.
func (s *Store) pendingOperationsFor(agentID string) ([]Operation, error) {
	ops, err := s.loadOperations()
	if err != nil {
		return nil, err
	}
	var out []Operation
	for _, r := range ops {
		if r.AgentID == agentID && r.Pending() {
			out = append(out, r.Operation)
		}
	}
	// Stable order so the ETag does not change just because a map iterated
	// differently, which would wake every agent for nothing.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RecordOperationResults applies the outcomes an agent reported.
//
// Idempotent and keyed by operation ID, which the crash-safety of the whole
// step depends on. The order is: apply the UI-side consequences, then
// record the result. A crash between them leaves the operation still
// published, the agent still re-reporting the same result, and this
// function free to run again -- whereas recording the result first would
// acknowledge the operation to the agent and lose the consequences forever.
func (s *Store) RecordOperationResults(agentID string, results []OperationResult) error {
	if len(results) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ops, err := s.loadOperations()
	if err != nil {
		return err
	}

	changed := false
	for _, res := range results {
		rec, ok := ops[res.ID]
		if !ok {
			// A result for something this UI no longer knows about. Not an
			// error: the agent re-sends until acknowledged, and an operation
			// deleted here is exactly that acknowledgement arriving late.
			continue
		}
		if rec.AgentID != agentID {
			// Only the agent an operation was published to may report on
			// it. Anything else is a bug or a compromised credential, and
			// silently accepting it would let one host close out another's
			// failover.
			continue
		}
		// Already fully processed.
		//
		// FollowUpDone is what makes this sufficient, and it is sufficient
		// only because it is set nowhere except under res.Terminal() below.
		// That gives the invariant FollowUpDone => the stored result is an
		// outcome, so this skips exactly the records that are finished with.
		// It is also why a stored "running" no longer stops the outcome
		// being recorded when it arrives: nothing set the flag.
		//
		// Anything that ever sets FollowUpDone elsewhere breaks that
		// invariant and reintroduces the bug this comment describes;
		// healStuckProgressRecord repairs the records the previous build
		// left behind that way.
		if rec.Result != nil && rec.FollowUpDone {
			continue
		}

		// The consequences, and the audit entry, belong to an OUTCOME.
		// Doing either on a progress report is what made the whole record
		// look closed: FollowUpDone was set although applyOperationFollowUp
		// had returned early with nothing to do, and the audit entry closed
		// with "running" as its outcome.
		if res.Terminal() {
			if !rec.FollowUpDone {
				if err := s.applyOperationFollowUp(rec, res); err != nil {
					return fmt.Errorf("apply the consequences of operation %s: %w", res.ID, err)
				}
				rec.FollowUpDone = true
			}
		}
		r := res
		rec.Result = &r
		ops[res.ID] = rec
		changed = true

		if res.Terminal() && rec.AuditID != "" {
			outcome := res.State
			if res.Error != "" {
				outcome = res.State + ": " + res.Error
			}
			// Best-effort: the audit entry is a record, not a gate, and
			// failing the whole report over it would cost the operational
			// state that actually matters.
			_ = s.completeAuditLocked(rec.AuditID, outcome)
		}
	}

	if !changed {
		return nil
	}
	return writeJSONAtomic(s.operationsPath(), ops, 0o600)
}

// applyOperationFollowUp makes the schedule agree with what just happened.
//
// This is the step whose absence silently breaks a pair. A promotion stops
// the old source replicating into a domain that is now live, and an
// inversion moves the whole relationship to the other host -- neither of
// which the schedule knows about, because the schedule is keyed by the
// agent that was the source when it was written.
func (s *Store) applyOperationFollowUp(rec OperationRecord, res OperationResult) error {
	if res.State != OpStateDone {
		return nil // nothing succeeded, so nothing to follow through
	}

	switch rec.Kind {
	case OpPromote:
		// The promoted domain is live now, and its old source must stop
		// syncing into it. Disabled rather than deleted: an inversion later
		// moves this entry, and deleting it would lose the profile an
		// operator tuned.
		//
		// promoted_from is what names that source, and the operation
		// carries it as PeerHost.
		if rec.PeerHost == "" {
			return nil
		}
		srcAgent, err := s.agentIDForHostLocked(rec.PeerHost)
		if err != nil || srcAgent == "" {
			// The old source has no agent here -- normal when its whole
			// site is gone, which is the usual reason for a forced
			// promotion. Nothing to disable.
			return nil
		}
		peerVM := rec.PeerVM
		if peerVM == "" {
			peerVM = rec.VM
		}
		return s.setScheduleEnabledLocked(srcAgent, peerVM, false)

	case OpInvert:
		// The inversion ran on the OLD source's agent, and the promoted
		// peer is the new source. The schedule entry has to follow the
		// direction, or the pair simply stops replicating with nothing
		// saying why.
		if rec.PeerHost == "" {
			return nil
		}
		newAgent, err := s.agentIDForHostLocked(rec.PeerHost)
		if err != nil {
			return err
		}
		if newAgent == "" {
			return fmt.Errorf("no enrolled agent for %s, so the schedule for %s cannot follow the inversion", rec.PeerHost, rec.VM)
		}
		peerVM := rec.PeerVM
		if peerVM == "" {
			peerVM = rec.VM
		}
		return s.moveScheduleEntryLocked(rec.AgentID, newAgent, rec.VM, peerVM)

	case OpRestore:
		// A restore rolls the replica's disks back and leaves replication
		// PAUSED, because the next sync from the same source would otherwise
		// overwrite exactly what was just rolled back to. The role stops it
		// at the far end; this stops the source trying at all.
		//
		// Disabled rather than deleted, like a promotion's: resuming means
		// re-enabling this same entry, and deleting it would lose the profile
		// somebody tuned.
		if rec.PeerHost == "" {
			// No peer recorded, so the source cannot be identified from here.
			// Not fatal: the replica's own paused role still refuses every
			// sync, so the interlock holds -- what is lost is only the
			// tidiness of the source not trying.
			return nil
		}
		srcAgent, err := s.agentIDForHostLocked(rec.PeerHost)
		if err != nil || srcAgent == "" {
			return nil
		}
		peerVM := rec.PeerVM
		if peerVM == "" {
			peerVM = rec.VM
		}
		return s.setScheduleEnabledLocked(srcAgent, peerVM, false)

	case OpReinit, OpForceClean:
		// The other half of going back to replicating after a restore. A
		// reinit runs on the SOURCE's agent and rebuilds the replica from
		// scratch, which is the only way back: a restore leaves metadata
		// describing an older checkpoint, so an incremental is refused by
		// design. A force-clean is the same journey for a replica too
		// wedged for a plain reinit to reach.
		//
		// Re-enabling on success rather than when it is issued: an entry
		// switched back on by a reinit that then failed would resume
		// scheduled syncs against a replica still in the state the operator
		// was trying to leave.
		return s.setScheduleEnabledLocked(rec.AgentID, rec.VM, true)
	}
	return nil
}

// agentIDForHostLocked resolves a hostname to an enrolled, non-revoked
// agent. Empty means none, which is not always an error.
func (s *Store) agentIDForHostLocked(host string) (string, error) {
	agents, err := s.loadAgents()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if !a.Revoked && strings.EqualFold(a.Hostname, host) {
			return a.ID, nil
		}
	}
	return "", nil
}

func newOperationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate an operation id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
