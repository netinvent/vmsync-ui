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

	OpStateDone    = "done"
	OpStateFailed  = "failed"
	OpStateExpired = "expired"
	OpStateRefused = "refused"
	OpStateUnknown = "unknown"
)

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
// A result of any kind ends publication -- that is what acknowledges it to
// the agent, which drops its own ledger record in turn. Cancelling ends it
// too. Expiry deliberately does NOT: the agent must still see it, refuse it
// and report that refusal, so the audit entry closes with a reason instead
// of hanging as "pending" forever.
func (r OperationRecord) Pending() bool {
	return r.Result == nil && r.CancelledAtUnix == 0
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
	return ops, nil
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
	if !rec.Pending() {
		// Already reported. Saying so beats pretending the cancel worked:
		// the operation has happened on a hypervisor and no amount of UI
		// state changes that.
		return fmt.Errorf("operation %s has already reported and cannot be cancelled", id)
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
		if rec.Result != nil && rec.FollowUpDone {
			continue // already fully processed
		}

		if !rec.FollowUpDone {
			if err := s.applyOperationFollowUp(rec, res); err != nil {
				return fmt.Errorf("apply the consequences of operation %s: %w", res.ID, err)
			}
			rec.FollowUpDone = true
		}
		r := res
		rec.Result = &r
		ops[res.ID] = rec
		changed = true

		if rec.AuditID != "" {
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
