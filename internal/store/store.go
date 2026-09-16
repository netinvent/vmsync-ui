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

// Package store persists the UI's state as JSON files on disk.
//
// Files rather than a database: the estate this manages is tens to low
// hundreds of VMs, the write rate is one report per agent per minute, and
// during an incident being able to read and edit the state with a text
// editor is worth more than query power. Every write goes through a
// temp-file-and-rename so a crash cannot leave a half-written file behind.
//
// Nothing here is the source of truth for replication itself. The topology
// lives in each domain's own libvirt metadata and is rediscovered by agents
// on every report; this store holds only what the UI adds on top --
// which agents are enrolled, what they last told us, and who did what.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Agent is one enrolled hypervisor.
type Agent struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	// TokenSHA256 is the hex digest of the agent's bearer token. The token
	// itself is shown once, at enrolment, and never stored: if this file
	// leaks, what leaks is a set of hashes rather than working credentials.
	TokenSHA256  string `json:"token_sha256"`
	EnrolledAt   int64  `json:"enrolled_at"`
	AgentVersion string `json:"agent_version,omitempty"`
	LastSeenAt   int64  `json:"last_seen_at,omitempty"`
	// Mode is what the agent last reported itself as: "monitor" or
	// "controlled". Empty until an agent that sends it has reported.
	//
	// Stored on the agent rather than left in the last report because it
	// gates what this console OFFERS, and the offer has to be right on a page
	// rendered while the agent is unreachable. Kept current by SaveReport,
	// which is also what makes a mode change visible: restart an agent as
	// --monitor and its next report says so.
	Mode string `json:"mode,omitempty"`
	// Revoked agents keep their record so the audit trail still resolves
	// their ID to a hostname long after they stop being able to connect.
	Revoked   bool  `json:"revoked,omitempty"`
	RevokedAt int64 `json:"revoked_at,omitempty"`
}

// The modes an agent can report itself as. They mirror cmd/vmsync-agent's own
// agentMode constants: another wire contract between two separately-versioned
// programs, written out rather than shared.
const (
	// AgentModeStandalone never actually arrives in a report -- a standalone
	// agent has no control plane to send one to. Named anyway so the set is
	// complete, and so a report carrying it is understood rather than treated
	// as an unknown string.
	AgentModeStandalone = "standalone"
	// AgentModeMonitor reports and does not act: no scheduler, no operations,
	// no fencing.
	AgentModeMonitor = "monitor"
	// AgentModeControlled runs what this console publishes.
	AgentModeControlled = "controlled"
)

// ReadOnly reports whether this console's instructions would be ignored by
// this agent.
//
// Deliberately false for an empty Mode. An agent that has never reported, or
// one whose report predates the field, has not told us it is a monitor -- and
// hiding controls from a host that would have honoured them is a worse failure
// than showing them for one that will not. The first is an operator who cannot
// act during an incident with no explanation; the second is an operator who
// acts, sees the warning this console shows beside a monitor agent, and knows
// why nothing happened.
func (a Agent) ReadOnly() bool { return a.Mode == AgentModeMonitor }

// EnrolmentToken is a single-use invitation for one named host.
type EnrolmentToken struct {
	TokenSHA256 string `json:"token_sha256"`
	Hostname    string `json:"hostname"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
	// UsedAt being non-zero is what makes this single-use. Spent tokens are
	// kept rather than deleted so "this token was already used" can be
	// distinguished from "this token never existed" in the audit trail.
	UsedAt      int64  `json:"used_at,omitempty"`
	UsedByAgent string `json:"used_by_agent,omitempty"`
	CreatedBy   string `json:"created_by"`
}

// Spent reports whether this token can no longer be redeemed.
func (t EnrolmentToken) Spent(now time.Time) bool {
	return t.UsedAt != 0 || (t.ExpiresAt != 0 && now.Unix() > t.ExpiresAt)
}

// Report is the inventory upload an agent sends. The field names mirror
// cmd/vmsync-agent's own Report type exactly -- this is a wire contract
// between two separately-versioned programs, so it is written out here
// rather than shared.
type Report struct {
	ReportedAtUnix   int64          `json:"reported_at_unix"`
	AgentVersion     string         `json:"agent_version"`
	Hostname         string         `json:"hostname"`
	LibvirtURI       string         `json:"libvirt_uri"`
	Domains          []ReportDomain `json:"domains"`
	ConfigAgeSeconds int64          `json:"config_age_seconds"`
	// Mode is which agent this is: "monitor" or "controlled". (An agent in
	// standalone mode has no control plane to report to, so that value never
	// arrives here, but it is not rejected either -- see AgentModeMonitor.)
	//
	// This console cannot work it out for itself, and the difference decides
	// whether anything it publishes will ever be read. A monitor agent runs
	// no scheduler and executes no operations: every control offered for one
	// would be accepted, stored, shown as pending, and never acted on. An
	// operator waiting on an instruction no process will ever read is the
	// worst failure this control plane has, because every part of it looks
	// like it worked.
	//
	// Empty from an agent that predates the field, which reads as "not
	// stated" rather than as any particular mode -- see Agent.ReadOnly.
	Mode string `json:"mode,omitempty"`
	// Syncs are recent scheduled-run outcomes, so an operator can see what
	// happened without reading a journal on a hypervisor.
	Syncs []SyncResult `json:"syncs,omitempty"`
	// OperationResults are the outcomes of one-shot operations, re-sent by
	// the agent on every report until this UI stops publishing the
	// operation. Repetition is the acknowledgement protocol; see
	// RecordOperationResults.
	//
	// UPGRADE ORDER: this field must exist here before any agent that sends
	// it is deployed. The report body is decoded with DisallowUnknownFields,
	// so an agent upgraded first has its ENTIRE report rejected -- domains,
	// roles, sync results and all -- not just this field. Upgrade the UI,
	// then the agents.
	OperationResults []OperationResult `json:"operation_results,omitempty"`
	// Filesystems is the storage behind the reported disks.
	Filesystems []ReportFilesystem `json:"filesystems,omitempty"`
	// EffectiveSchedule is what the agent will ACTUALLY do, after resolving
	// templates and synthesising entries from the default.
	//
	// This console must not compute it. Templates resolve in the agent --
	// a standalone agent has no control plane at all, so resolution cannot
	// live here -- which means what this UI published and what the agent runs
	// are two different documents. Rendering the published one as though it
	// were the running one is the failure this field closes.
	EffectiveSchedule []EffectiveScheduleEntry `json:"effective_schedule,omitempty"`
	// Timezone is the agent host's own zone, and the one every verify window
	// below is expressed in. Displayed, never chosen: a window means the quiet
	// hours where the disks are, so each host is on its own clock.
	Timezone string `json:"timezone,omitempty"`
	// TimezoneOffsetSeconds is that zone's offset from UTC at the moment of
	// the report, so a window can be rendered without carrying tzdata here and
	// without guessing at DST.
	TimezoneOffsetSeconds int `json:"timezone_offset_seconds,omitempty"`
}

// EffectiveScheduleEntry is one VM's schedule as the AGENT resolved it. The
// field names mirror cmd/vmsync-agent's own type exactly -- a wire contract
// between two separately-versioned programs, written out rather than shared.
type EffectiveScheduleEntry struct {
	VM string `json:"vm"`
	// Template is the template this entry inherited from, already resolved, so
	// an entry naming none reads "default" rather than empty.
	Template string `json:"template,omitempty"`
	// Synthesised is an entry the AGENT invented from the default template
	// because this VM had none of its own.
	//
	// The console has to show this distinctly. Such a VM has no ScheduleEntry
	// here at all, so without it the page reports "not scheduled" about a VM
	// the agent is replicating on a timer -- the exact inversion of the truth
	// that this whole report exists to prevent.
	Synthesised     bool `json:"synthesised,omitempty"`
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds,omitempty"`

	VerifyMode            string `json:"verify_mode,omitempty"`
	VerifyIntervalSeconds int    `json:"verify_interval_seconds,omitempty"`
	VerifyDays            string `json:"verify_days,omitempty"`
	VerifyWindow          string `json:"verify_window,omitempty"`
	// NextVerifyUnix is when the verify window next opens, resolved in the
	// agent's own timezone. 0 when there is no calendar.
	NextVerifyUnix int64 `json:"next_verify_unix,omitempty"`
	// VerifyWindowOpen says the window is open right now.
	VerifyWindowOpen bool `json:"verify_window_open,omitempty"`
	// CalendarError is why the calendar could not be parsed, when it could
	// not. Such an entry never verifies, and that must be visible here rather
	// than only in the agent's journal.
	CalendarError string `json:"calendar_error,omitempty"`
}

// SyncResult is one scheduled run's outcome, as the agent reported it.
type SyncResult struct {
	VM             string `json:"vm"`
	TargetHost     string `json:"target_host,omitempty"`
	StartedAtUnix  int64  `json:"started_at_unix"`
	FinishedAtUnix int64  `json:"finished_at_unix"`
	DurationSecs   int64  `json:"duration_seconds"`
	// ExitCode is a POINTER so that "the agent never observed one" is a
	// different value from "it exited 0".
	//
	// A plain int made those identical, and the difference is not academic:
	// an adopted run -- one a previous agent instance started and this one
	// merely found still going -- can never have its exit status read,
	// because it is not this process's child. Reporting that as 0 would put
	// a green tick on a run whose outcome nobody knows.
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	LogTail  string `json:"log_tail,omitempty"`

	// RunID joins this result to the agent's own run log, so an operator
	// reading a row here can grep the host for exactly that launch.
	RunID string `json:"run_id,omitempty"`
	// Degraded says the run SUCCEEDED but something about it needs a person.
	//
	// Separate from Outcome because it is orthogonal: a sync can copy every
	// byte correctly and still leave the source guest with its filesystems
	// frozen, or produce a replica that is only crash-consistent. Both of
	// those exit 0, and before this field the console showed them as an
	// unqualified green tick -- which is how a hung production VM stayed
	// invisible to everyone not watching Prometheus.
	Degraded bool `json:"degraded,omitempty"`
	// DegradedReason is what to tell the operator, in their words rather
	// than a code they have to look up.
	DegradedReason string `json:"degraded_reason,omitempty"`
	// Outcome is the agent's own word for how it went: success, failure,
	// busy, or unknown. Carried rather than re-derived here, because the
	// agent knows things this side cannot -- "busy" means vmsync stood down
	// on lock contention without touching anything, which is neither a
	// success nor a failure and would be mis-rendered as either.
	Outcome string `json:"outcome,omitempty"`
}

// Outcome values, matching what the agent sends.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeBusy    = "busy"
	OutcomeUnknown = "unknown"
)

// Succeeded reports whether this run is known to have worked.
//
// Deliberately three-valued underneath: anything that is not positively a
// success is not treated as one. An unobserved exit code reads as unknown,
// never as fine.
func (r SyncResult) Succeeded() bool {
	if r.Outcome != "" {
		return r.Outcome == OutcomeSuccess
	}
	// An agent older than the Outcome field. Fall back to the exit code,
	// which for those agents is always populated.
	return r.ExitCode != nil && *r.ExitCode == 0 && r.Error == ""
}

// Unobserved reports whether the agent never saw how this run ended.
func (r SyncResult) Unobserved() bool {
	return r.Outcome == OutcomeUnknown || r.ExitCode == nil
}

// ReportDomain is one domain's state as an agent found it.
type ReportDomain struct {
	Name           string   `json:"name"`
	UUID           string   `json:"uuid,omitempty"`
	Active         bool     `json:"active"`
	Role           string   `json:"role,omitempty"`
	LastCheckpoint string   `json:"last_checkpoint,omitempty"`
	LastSyncUnix   int64    `json:"last_sync_unix,omitempty"`
	FailureCount   int      `json:"failure_count"`
	ReplicaSource  string   `json:"replica_source,omitempty"`
	ReplicaTargets []string `json:"replica_targets,omitempty"`
	// The promotion record, present only on a domain failed over TO.
	// PromotedFrom names the source a promotion displaced, which is what
	// lets this UI work out who must not keep running alongside it.
	PromotedFrom   string `json:"promoted_from,omitempty"`
	PromotedAtUnix int64  `json:"promoted_at_unix,omitempty"`
	PromotedBy     string `json:"promoted_by,omitempty"`
	PromotionMode  string `json:"promotion_mode,omitempty"`
	// LastReplicatedAtUnix / LastReplicatedTo are the SOURCE side of what
	// LastSyncUnix records on a target -- the same fact from the other end,
	// so the question survives losing one of the two hosts.
	LastReplicatedAtUnix int64  `json:"last_replicated_at_unix,omitempty"`
	LastReplicatedTo     string `json:"last_replicated_to,omitempty"`
	// The fence this domain's promotion armed, present only on a promoted
	// domain whose promotion asked for one. Absence is meaningful: a
	// promotion carrying no fence authorised nothing, which is what a DR
	// drill looks like.
	FenceID          string `json:"fence_id,omitempty"`
	FenceSource      string `json:"fence_source,omitempty"`
	FenceArmedAtUnix int64  `json:"fence_armed_at_unix,omitempty"`
	FenceArmedBy     string `json:"fence_armed_by,omitempty"`
	// Fenced is what the reporting agent DID about a fence naming this
	// domain, from its own durable ledger.
	//
	// The half of the picture nothing here could derive. A fence that
	// worked leaves a domain that is merely `paused`, indistinguishable
	// from one an operator paused by hand; a fence that FAILED leaves no
	// trace in libvirt at all, just a VM still running beside a promoted
	// copy. Both were previously visible only in the agent's own logs and
	// Prometheus metrics.
	Fenced *ReportFenced `json:"fenced,omitempty"`

	Disks []ReportDisk `json:"disks,omitempty"`
	// RestorePoints is what this replica can be rolled back to, newest
	// first.
	//
	// UPGRADE ORDER: this field must exist here before any agent that sends
	// it is deployed. The report body is decoded with DisallowUnknownFields,
	// so an agent upgraded first has its ENTIRE report rejected -- domains,
	// roles, sync results and all -- not just this field. Upgrade the UI,
	// then the agents.
	//
	// It is here because a restore operation names a TAG, and unlike every
	// other operation parameter -- a role, a peer -- a tag cannot be derived
	// from anything else reported: restore points are directories on the
	// target's filesystem, deliberately, because vmsync keeps no inventory of
	// its own for them. Without this the UI could only offer a restore point
	// it has never seen.
	// The restore record: this replica's disks were rolled back to one of its
	// restore points. It SURVIVES a promotion, which is what makes it worth
	// having: afterwards, role=paused has been overwritten and every other
	// signal of a rollback is ambiguous with an ordinary lagging replica.
	//
	// UPGRADE ORDER: as with RestorePoints above, this must exist here before
	// an agent that sends it is deployed.
	RestoredFrom   string               `json:"restored_from,omitempty"`
	RestoredAtUnix int64                `json:"restored_at_unix,omitempty"`
	RestoredBy     string               `json:"restored_by,omitempty"`
	RestorePoints  []ReportRestorePoint `json:"restore_points,omitempty"`
	Status         string               `json:"status"`
	Reasons        []string             `json:"reasons,omitempty"`
	AgeSeconds     int64                `json:"age_seconds"`
}

// SyncProfile mirrors the agent's own type field for field. It is a wire
// contract between two separately-versioned programs, so it is written out
// here rather than shared, and changes only when the protocol does.
//
// Note what is absent: no flag names, no command line, no credentials. The
// UI describes intent; the agent owns how that is spelled and supplies
// credentials from its own local configuration.
type SyncProfile struct {
	Compress      string `json:"compress,omitempty"`
	CompressLevel string `json:"compress_level,omitempty"`
	NetBuffer     string `json:"netbuffer,omitempty"`
	UseSSH        bool   `json:"use_ssh,omitempty"`
	IODepth       int    `json:"io_depth,omitempty"`
	// NoChecksum disables vmsync's pre-commit integrity check, which is on
	// by default: the copy hashes every chunk it reads, vmsync-bridge-helper
	// hashes the same ranges back off the target, and an incremental sync's
	// overlay is removed instead of committed if they disagree.
	//
	// Negative, mirroring the agent field and vmsync's own -no-checksum, so
	// that a profile which does not mention it leaves the check ON. A
	// positive "checksum" would make every profile written before this field
	// existed read as disabling a safety feature.
	NoChecksum bool   `json:"no_checksum,omitempty"`
	Verify     string `json:"verify,omitempty"`
	// VerifyFailureReinit answers a verification failure with one full recopy
	// and a second verification; if that fails too, the replica is recorded as
	// faulty and both syncs into it and promotions of it are refused until a
	// human clears it.
	//
	// Only meaningful with Verify set, and the agent refuses the combination
	// otherwise rather than ignoring it. Kept as a separate boolean rather than
	// folded into Verify as a fourth mode because it is a different axis:
	// Verify chooses how the comparison is made, this chooses what happens when
	// it fails, and every mode can have either answer.
	VerifyFailureReinit bool   `json:"verify_failure_reinit,omitempty"`
	ReinitAfterFailures int    `json:"reinit_after_failures,omitempty"`
	TargetDiskPath      string `json:"target_disk_path,omitempty"`
	// Retention is "<count>,<interval>" (e.g. "24,3h"), empty to keep no
	// restore points. Without it a scheduled sync never passes -retention,
	// so an estate run through this UI takes no restore points at all -- and
	// a restore has nothing to restore.
	Retention string `json:"retention,omitempty"`
	// TimestampToleranceSec is how far a replica disk's mtime may be ahead
	// of the last sync timestamp before vmsync refuses. Those are two
	// different hosts' clocks, so a target running a second fast otherwise
	// fails every scheduled sync for that pair with an error blaming
	// out-of-band modification.
	TimestampToleranceSec int    `json:"timestamp_tolerance_sec,omitempty"`
	SourcePortRange       string `json:"source_port_range,omitempty"`
	TargetPortRange       string `json:"target_port_range,omitempty"`
}

// ScheduleEntry is one VM an agent should sync, and how often.
type ScheduleEntry struct {
	VM              string      `json:"vm"`
	IntervalSeconds int         `json:"interval_seconds"`
	Enabled         bool        `json:"enabled"`
	Profile         SyncProfile `json:"profile"`
	TargetHost      string      `json:"target_host,omitempty"`
	// VerifyIntervalSeconds is how often a scheduled sync should ALSO verify,
	// 0 meaning "whatever Profile.Verify says, on every run".
	//
	// It exists because verification was otherwise all-or-nothing per pair.
	// Profile.Verify is a profile field, so either every sync of this VM
	// verified or none did -- and a verify is a full-image read on both sides.
	// An operator wanting verification at all had to choose between paying it
	// every fifteen minutes, stretching the interval and losing RPO, or not
	// verifying. None of those is an answer.
	//
	// It is a cadence for WHICH SYNCS ALSO VERIFY, not a second schedule, and
	// that is forced rather than chosen: -verify compares the target against
	// the backup job's export the copy read from, so it is a phase of a sync
	// and there is no such thing as a verify-only run. The agent keeps a
	// second due-time per VM and sets Verify on the next sync that falls due
	// after it, which needs no new run type and lands the verify on a run
	// that was going to happen anyway.
	//
	// Requires Profile.Verify to name a mode -- this says how often, not
	// what. Rejected at validation otherwise, so a profile cannot promise a
	// cadence for something it never does.
	VerifyIntervalSeconds int `json:"verify_interval_seconds,omitempty"`
	// VerifyDays and VerifyWindow are the CALENDAR form of the same cadence:
	// "Sun *-*-01..07" with "02:00-12:00" is the first Sunday of the month,
	// between 2am and noon. A deliberately small subset of systemd's
	// OnCalendar -- see the agent's pkg/schedcal, which owns the grammar.
	//
	// An interval cannot express that. "Every 30 days" drifts off the weekend
	// within two months, and a verify is the one operation an estate wants
	// pinned to a quiet window: it is a full-image read on both sides, so
	// landing it on a Tuesday afternoon is exactly what an operator is trying
	// to avoid.
	//
	// This and VerifyIntervalSeconds are two spellings of one setting, so the
	// agent REFUSES an entry carrying both -- there is no precedence an
	// operator could predict. The console must not publish both either.
	//
	// Resolved in the AGENT'S local time, from its own host clock, and the
	// console displays the zone it is told rather than choosing one: a window
	// means the quiet hours where the disks are, and an estate spanning zones
	// wants each host's night, not this server's.
	VerifyDays   string `json:"verify_days,omitempty"`
	VerifyWindow string `json:"verify_window,omitempty"`
	// Template names the ScheduleTemplate this entry inherits unset fields
	// from; empty means "default". The fields above become OVERRIDES once a
	// template is in play -- 0 or "" inherits.
	//
	// Enabled is the exception and is never inherited, because false cannot be
	// told from unset in a bool. That is also the opt-out: a VM covered by the
	// default that should NOT be replicated on a schedule gets an explicit
	// entry with Enabled false.
	Template string `json:"template,omitempty"`
	// ShutdownTimeoutSec overrides the estate default for THIS VM, 0 to use
	// it. Per-VM because how long a guest takes to stop cleanly is a
	// property of what it runs: a stateless web front end is gone in
	// seconds, and a database flushing buffers is not.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`
	// Preset records which built-in the profile came from, purely so the UI
	// can show it and offer to re-apply. The agent neither receives nor
	// cares about this -- it acts on the resolved fields above.
	Preset string `json:"-"`
}

// AgentConfig is what the UI hands one agent. Schedule is per-agent;
// everything else is estate-wide.
type AgentConfig struct {
	ReportIntervalSeconds int            `json:"report_interval_seconds"`
	PollWaitSeconds       int            `json:"poll_wait_seconds"`
	CadenceSeconds        map[string]int `json:"cadence_seconds,omitempty"`

	Schedule []ScheduleEntry `json:"schedule,omitempty"`
	// Templates are the named cadences entries inherit from, estate-wide.
	//
	// Sent UNRESOLVED. The agent resolves them (ResolveSchedule), because a
	// standalone agent has no control plane to resolve on its behalf -- see
	// docs/design/scheduling.md. This UI therefore must not predict the
	// result; it displays the effective schedule the agent reports back.
	//
	// A template named "default" additionally covers every syncable VM with
	// no entry of its own, and its ABSENCE is the feature's off switch.
	Templates              map[string]ScheduleTemplate `json:"templates,omitempty"`
	Operations             []Operation                 `json:"operations,omitempty"`
	MaxConcurrentSyncs     int                         `json:"max_concurrent_syncs,omitempty"`
	TargetReplicationSlots map[string]int              `json:"target_replication_slots,omitempty"`
	// ShutdownTimeoutSec is the estate default, for the agent to fall back
	// on when it shuts a domain down with no per-VM value to use.
	//
	// Served as well as carried on each operation because the agent shuts
	// domains down on its OWN initiative too: a fence has no operation
	// behind it, and during the partition that usually motivates one there
	// may be no control plane to ask.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`
}

// Settings are the estate-wide knobs, stored once.
type Settings struct {
	ReportIntervalSeconds  int            `json:"report_interval_seconds"`
	PollWaitSeconds        int            `json:"poll_wait_seconds"`
	MaxConcurrentSyncs     int            `json:"max_concurrent_syncs"`
	TargetReplicationSlots map[string]int `json:"target_replication_slots,omitempty"`
	// ShutdownTimeoutSec is how long a clean guest shutdown may take before
	// vmsync gives up, estate-wide, for any VM with no value of its own.
	//
	// It matters more than it looks. A shutdown that overruns is reported as
	// a FAILURE, and when it was a fence that asked for it, fences latch --
	// so nothing tries again, and the console shows a live split brain that
	// is really just a database taking its time. Set it to the slowest thing
	// you would still wait for.
	ShutdownTimeoutSec int `json:"shutdown_timeout_sec,omitempty"`
	// Templates are the named cadences schedule entries inherit from, keyed
	// by name. Estate-wide, like everything else here.
	//
	// A template called "default" additionally covers every syncable VM with
	// no entry of its own, which is the point of the feature: without it a
	// newly discovered VM is not replicated until somebody creates its entry,
	// so the cost of forgetting is "not protected", silently.
	//
	// Its ABSENCE is the off switch. No default means no such coverage, which
	// is the behaviour that predates templates -- so an estate upgrading does
	// not silently begin syncing VMs nobody scheduled.
	Templates map[string]ScheduleTemplate `json:"templates,omitempty"`
}

// ScheduleTemplate is a named cadence and profile that entries inherit from.
//
// The field names mirror cmd/vmsync-agent's own ScheduleTemplate exactly:
// this is a wire contract between two separately-versioned programs, so it is
// written out here rather than shared -- and this UI deliberately depends on
// nothing outside the standard library, so it could not import it anyway.
//
// It carries a concrete Profile rather than naming a Preset. Presets are a UI
// concept the agent has never heard of (ScheduleEntry.Preset is `json:"-"`),
// so a template referencing one could not be resolved by the component that
// does the resolving. The UI fills this Profile FROM a preset at authoring
// time and keeps the preset name in Preset for its own "re-apply" offer.
type ScheduleTemplate struct {
	Name                  string `json:"name"`
	IntervalSeconds       int    `json:"interval_seconds"`
	VerifyIntervalSeconds int    `json:"verify_interval_seconds,omitempty"`
	// VerifyDays and VerifyWindow are the calendar form of the verify cadence
	// -- see ScheduleEntry.VerifyDays. This is the field templates were most
	// worth building for: a verify window is an estate-wide policy ("first
	// Sunday of the month, overnight") far more often than a per-VM one, and
	// restating a calendar expression on every entry is how an estate ends up
	// with three subtly different ones.
	VerifyDays   string      `json:"verify_days,omitempty"`
	VerifyWindow string      `json:"verify_window,omitempty"`
	Profile      SyncProfile `json:"profile"`
	Enabled      bool        `json:"enabled"`
	// Preset is UI-only, like ScheduleEntry.Preset: it records which built-in
	// the profile came from so the console can offer to re-apply it, and the
	// agent neither receives it nor cares.
	Preset string `json:"-"`
}

func DefaultSettings() Settings {
	// MaxConcurrentSyncs matches the agent's own defaultMaxConcurrent. It has
	// to: this value is sent to every enrolled agent and takes precedence
	// over the agent's built-in default, so a lower number here would
	// silently hold the whole fleet below it while the agent's own
	// documentation said otherwise. Agents clamp it regardless, and a host
	// can lower it further with -max-concurrent-syncs.
	return Settings{
		ReportIntervalSeconds: 60, PollWaitSeconds: 30, MaxConcurrentSyncs: 4,
		ShutdownTimeoutSec: DefaultShutdownTimeoutSec,
	}
}

// Shutdown timeout bounds.
const (
	// DefaultShutdownTimeoutSec matches vmsync's own -shutdown-timeout-sec
	// default. Deliberately the same number: this value is sent to every
	// agent and overrides what vmsync would otherwise choose, so a different
	// default here would quietly change the behaviour of every shutdown in
	// the estate the moment a control plane was introduced.
	DefaultShutdownTimeoutSec = 300
	// MinShutdownTimeoutSec. Below about this, an ACPI request has not had
	// time to reach a guest's init system, let alone be acted on -- so a
	// smaller number does not make shutdowns faster, it makes them fail.
	MinShutdownTimeoutSec = 30
	// MaxShutdownTimeoutSec. An hour is far longer than any guest should
	// need, and the cap exists so a mistyped value cannot wedge a failover
	// behind a wait nobody intended.
	MaxShutdownTimeoutSec = 3600
)

// ResolveShutdownTimeout picks the timeout for one VM: its own value, the
// estate default, or vmsync's -- in that order.
//
// One function so the answer cannot differ between the operation path and
// the fence path. Those two shut a domain down in exactly the same way, and
// a VM that stops fine when an operator asks but fails when a fence does
// would be a genuinely baffling thing to debug.
func ResolveShutdownTimeout(perVM, estate int) int {
	if perVM > 0 {
		return perVM
	}
	if estate > 0 {
		return estate
	}
	return DefaultShutdownTimeoutSec
}

// ValidateShutdownTimeout accepts 0 (meaning "inherit") or a value in range.
func ValidateShutdownTimeout(sec int) error {
	if sec == 0 {
		return nil
	}
	if sec < MinShutdownTimeoutSec || sec > MaxShutdownTimeoutSec {
		return fmt.Errorf("a shutdown timeout of %ds is out of range: use 0 to inherit, or %d to %d seconds",
			sec, MinShutdownTimeoutSec, MaxShutdownTimeoutSec)
	}
	return nil
}

// DefaultAgentConfig matches the agent's own built-in defaults, so an agent
// that has never reached the UI behaves the same as one that has.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{ReportIntervalSeconds: 60, PollWaitSeconds: 30}
}

// AuditEntry records one thing a person did. Written before the action and
// updated after: a failover that half-completes is exactly the case that
// most needs attribution, and log-on-success loses precisely those.
type AuditEntry struct {
	ID       string `json:"id"`
	AtUnix   int64  `json:"at_unix"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Target   string `json:"target,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	DoneUnix int64  `json:"done_unix,omitempty"`
}

// Store holds every piece of UI state under one directory.
//
// A single mutex serializes all access. At this scale that is not a
// bottleneck -- a report per agent per minute -- and it removes an entire
// class of bug from a program whose whole job is to be trustworthy about
// state.
type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o700); err != nil {
		return nil, fmt.Errorf("create reports directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// --- agents ---------------------------------------------------------------

func (s *Store) Agents() ([]Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadAgents()
}

func (s *Store) loadAgents() ([]Agent, error) {
	var agents []Agent
	if _, err := readJSON(s.path("agents.json"), &agents); err != nil {
		return nil, err
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Hostname < agents[j].Hostname })
	return agents, nil
}

// CreateEnrolmentToken mints a single-use token for hostname and returns
// the clear-text value, which is the only time it exists in that form.
func (s *Store) CreateEnrolmentToken(hostname, createdBy string, ttl time.Duration) (string, error) {
	if hostname == "" {
		return "", fmt.Errorf("an enrolment token must name the host it is for")
	}
	clear, err := randomToken()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var tokens []EnrolmentToken
	if _, err := readJSON(s.path("enrolment-tokens.json"), &tokens); err != nil {
		return "", err
	}
	now := time.Now()
	tokens = append(tokens, EnrolmentToken{
		TokenSHA256: hashToken(clear),
		Hostname:    hostname,
		CreatedAt:   now.Unix(),
		ExpiresAt:   now.Add(ttl).Unix(),
		CreatedBy:   createdBy,
	})
	if err := writeJSONAtomic(s.path("enrolment-tokens.json"), tokens, 0o600); err != nil {
		return "", err
	}
	return clear, nil
}

// ErrEnrolment is returned for every enrolment failure, deliberately
// without saying which one: an unauthenticated caller learns only that it
// did not work, not whether the token was wrong, expired, spent, or meant
// for a different host.
var ErrEnrolment = fmt.Errorf("enrolment refused")

// Enrol redeems a token and registers the agent, returning the new agent
// and its clear-text bearer token.
func (s *Store) Enrol(clearToken, hostname, agentVersion string) (Agent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tokens []EnrolmentToken
	if _, err := readJSON(s.path("enrolment-tokens.json"), &tokens); err != nil {
		return Agent{}, "", err
	}
	digest := hashToken(clearToken)
	now := time.Now()

	idx := -1
	for i := range tokens {
		// Constant-time compare: the digests are not secret, but comparing
		// them with == leaks position through timing, and there is no reason
		// to accept that when the alternative is one function call.
		if subtle.ConstantTimeCompare([]byte(tokens[i].TokenSHA256), []byte(digest)) == 1 {
			idx = i
			break
		}
	}
	if idx < 0 || tokens[idx].Spent(now) {
		return Agent{}, "", ErrEnrolment
	}
	// A token is minted for one named host. Honouring it for a different
	// hostname would turn a leaked token into an enrolment anywhere.
	if !strings.EqualFold(tokens[idx].Hostname, hostname) {
		return Agent{}, "", ErrEnrolment
	}

	bearer, err := randomToken()
	if err != nil {
		return Agent{}, "", err
	}
	agents, err := s.loadAgents()
	if err != nil {
		return Agent{}, "", err
	}
	id, err := randomID()
	if err != nil {
		return Agent{}, "", err
	}
	agent := Agent{
		ID:           id,
		Hostname:     hostname,
		TokenSHA256:  hashToken(bearer),
		EnrolledAt:   now.Unix(),
		AgentVersion: agentVersion,
	}
	agents = append(agents, agent)

	tokens[idx].UsedAt = now.Unix()
	tokens[idx].UsedByAgent = agent.ID

	if err := writeJSONAtomic(s.path("agents.json"), agents, 0o600); err != nil {
		return Agent{}, "", err
	}
	if err := writeJSONAtomic(s.path("enrolment-tokens.json"), tokens, 0o600); err != nil {
		return Agent{}, "", err
	}
	return agent, bearer, nil
}

// Authenticate resolves an agent ID and bearer token to an agent record.
// A revoked agent authenticates as not-found: the caller answers 401 either
// way, and there is no reason to tell a rejected client which it was.
func (s *Store) Authenticate(agentID, bearer string) (Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agents, err := s.loadAgents()
	if err != nil {
		return Agent{}, false
	}
	digest := hashToken(bearer)
	for _, a := range agents {
		if a.ID != agentID || a.Revoked {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(a.TokenSHA256), []byte(digest)) == 1 {
			return a, true
		}
	}
	return Agent{}, false
}

// Revoke stops an agent being able to connect. The record is kept so the
// audit trail can still resolve its ID to a hostname.
func (s *Store) Revoke(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	agents, err := s.loadAgents()
	if err != nil {
		return err
	}
	found := false
	for i := range agents {
		if agents[i].ID == agentID {
			agents[i].Revoked = true
			agents[i].RevokedAt = time.Now().Unix()
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no agent with id %q", agentID)
	}
	return writeJSONAtomic(s.path("agents.json"), agents, 0o600)
}

// --- reports --------------------------------------------------------------

// SaveReport stores an agent's latest inventory, replacing the previous
// one, and stamps the agent as seen.
func (s *Store) SaveReport(agentID string, r Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := writeJSONAtomic(s.reportPath(agentID), r, 0o600); err != nil {
		return err
	}
	agents, err := s.loadAgents()
	if err != nil {
		return err
	}
	for i := range agents {
		if agents[i].ID == agentID {
			agents[i].LastSeenAt = time.Now().Unix()
			if r.AgentVersion != "" {
				agents[i].AgentVersion = r.AgentVersion
			}
			// Guarded the same way, and for the same reason: an empty
			// value means "this agent did not say", not "this agent
			// changed to nothing". Overwriting on empty would lose a
			// known mode to one report from an older binary, and what
			// it would lose is the thing gating whether this console
			// offers controls that would be silently ignored.
			if r.Mode != "" {
				agents[i].Mode = r.Mode
			}
		}
	}
	return writeJSONAtomic(s.path("agents.json"), agents, 0o600)
}

// Report returns an agent's latest inventory, ok=false when it has never
// reported.
func (s *Store) Report(agentID string) (Report, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Report
	ok, err := readJSON(s.reportPath(agentID), &r)
	return r, ok, err
}

func (s *Store) reportPath(agentID string) string {
	// Agent IDs are hex from randomID, but this path is built from a value
	// that arrived over the network -- derive the filename from a hash so a
	// crafted ID can never escape the reports directory.
	return filepath.Join(s.dir, "reports", hashToken(agentID)+".json")
}

// --- agent configuration --------------------------------------------------

// Settings returns the estate-wide knobs.
func (s *Store) Settings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadSettings()
}

func (s *Store) loadSettings() (Settings, error) {
	set := DefaultSettings()
	if _, err := readJSON(s.path("settings.json"), &set); err != nil {
		return Settings{}, err
	}
	return set, nil
}

func (s *Store) SetSettings(set Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONAtomic(s.path("settings.json"), set, 0o600)
}

// Schedules returns every agent's schedule, keyed by agent ID.
func (s *Store) Schedules() (map[string][]ScheduleEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadSchedules()
}

func (s *Store) loadSchedules() (map[string][]ScheduleEntry, error) {
	sched := map[string][]ScheduleEntry{}
	if _, err := readJSON(s.path("schedules.json"), &sched); err != nil {
		return nil, err
	}
	return sched, nil
}

// SetScheduleEntry adds or replaces one VM's entry for one agent.
func (s *Store) SetScheduleEntry(agentID string, entry ScheduleEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, err := s.loadSchedules()
	if err != nil {
		return err
	}
	entries := sched[agentID]
	replaced := false
	for i := range entries {
		if entries[i].VM == entry.VM {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].VM < entries[j].VM })
	sched[agentID] = entries
	return writeJSONAtomic(s.path("schedules.json"), sched, 0o600)
}

// DeleteScheduleEntry removes one VM's entry.
func (s *Store) DeleteScheduleEntry(agentID, vm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, err := s.loadSchedules()
	if err != nil {
		return err
	}
	out := sched[agentID][:0]
	for _, e := range sched[agentID] {
		if e.VM != vm {
			out = append(out, e)
		}
	}
	sched[agentID] = out
	return writeJSONAtomic(s.path("schedules.json"), sched, 0o600)
}

// setScheduleEnabledLocked flips one entry's Enabled flag, leaving
// everything else about it alone.
//
// Disabling rather than deleting matters: the profile an operator tuned --
// compression, verification, disk path -- is worth keeping through a
// failover, and an inversion later moves this same entry rather than
// needing it rebuilt from memory.
func (s *Store) setScheduleEnabledLocked(agentID, vm string, enabled bool) error {
	sched, err := s.loadSchedules()
	if err != nil {
		return err
	}
	entries := sched[agentID]
	found := false
	for i := range entries {
		if entries[i].VM == vm {
			entries[i].Enabled = enabled
			found = true
		}
	}
	if !found {
		return nil // nothing scheduled for it; nothing to disable
	}
	sched[agentID] = entries
	return writeJSONAtomic(s.path("schedules.json"), sched, 0o600)
}

// moveScheduleEntryLocked moves a VM's schedule from one agent to another in
// a SINGLE write.
//
// One write, not a delete followed by a set, because there is no
// transaction spanning two of them: a crash in between would leave the
// entry under neither agent, and an absent schedule entry is
// indistinguishable from a VM nobody asked to replicate. The pair would
// simply stop, silently, immediately after a failover -- the moment it must
// not.
//
// The migrated entry arrives DISABLED. The first sync in the reversed
// direction has no checkpoint chain and must be a full reinit, which the
// schedule cannot yet express; leaving it enabled would schedule a run that
// fails every interval. Disabled, an operator re-enables it once they have
// done that first full sync, and vmsync's own error says exactly that.
func (s *Store) moveScheduleEntryLocked(fromAgentID, toAgentID, vm, newVM string) error {
	sched, err := s.loadSchedules()
	if err != nil {
		return err
	}

	var moved *ScheduleEntry
	remaining := sched[fromAgentID][:0]
	for _, e := range sched[fromAgentID] {
		if e.VM == vm && moved == nil {
			cp := e
			moved = &cp
			continue
		}
		remaining = append(remaining, e)
	}
	if moved == nil {
		return nil // nothing scheduled here; the inversion still stands
	}
	sched[fromAgentID] = remaining

	// The old source's host is what the new source now replicates to.
	fromHost, err := s.hostnameForAgentLocked(fromAgentID)
	if err != nil {
		return err
	}
	moved.VM = newVM
	moved.TargetHost = fromHost
	moved.Enabled = false

	// Point the reversed direction at where the NEW target's disks actually
	// are, rather than carrying the old direction's target_disk_path across.
	//
	// That value describes where THIS pair's replicas went, which after an
	// inversion is where the new SOURCE's disks live -- so reusing it aims
	// the reversed copy at the wrong place on the wrong host. With an
	// asymmetric layout (source in /var/lib/libvirt/images, replicas in
	// /data/replicas) the first reversed sync would either fail outright
	// because that directory does not exist on the old source, or -- where
	// it does exist -- write the replica there and redefine the domain to
	// match, silently orphaning the original disk.
	//
	// The old source is fromAgentID, and its own report says where its disks
	// are. Left untouched when that cannot be reduced to a single directory:
	// target_disk_path is one directory for every disk, so a domain whose
	// disks span several cannot be expressed by it in either direction, and
	// guessing one of them would be worse than leaving a value an operator
	// can see and correct. The entry arrives disabled either way.
	if dir, ok := s.singleDiskDirLocked(fromAgentID, vm); ok {
		moved.Profile.TargetDiskPath = dir
	}

	entries := sched[toAgentID]
	replaced := false
	for i := range entries {
		if entries[i].VM == moved.VM {
			entries[i] = *moved
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, *moved)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].VM < entries[j].VM })
	sched[toAgentID] = entries

	return writeJSONAtomic(s.path("schedules.json"), sched, 0o600)
}

// singleDiskDirLocked reports the one directory holding every disk of a
// domain, as its own agent last reported them.
//
// False when there is no report, no domain by that name, no disks, or the
// disks span more than one directory -- each of which means "this cannot be
// expressed as a single target_disk_path", and the caller leaves the
// existing value alone rather than picking one.
func (s *Store) singleDiskDirLocked(agentID, vm string) (string, bool) {
	var rep Report
	ok, err := readJSON(s.reportPath(agentID), &rep)
	if err != nil || !ok {
		return "", false
	}
	for _, d := range rep.Domains {
		if !strings.EqualFold(d.Name, vm) {
			continue
		}
		dir := ""
		for _, disk := range d.Disks {
			if disk.Path == "" {
				continue
			}
			// path, not filepath: these are paths on a Linux hypervisor, and
			// this program may well be running on something else.
			this := path.Dir(disk.Path)
			if dir == "" {
				dir = this
				continue
			}
			if dir != this {
				return "", false
			}
		}
		if dir == "" {
			return "", false
		}
		return dir, true
	}
	return "", false
}

func (s *Store) hostnameForAgentLocked(agentID string) (string, error) {
	agents, err := s.loadAgents()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.ID == agentID {
			return a.Hostname, nil
		}
	}
	return "", nil
}

// AgentConfigFor builds the configuration for one agent, with an ETag that
// changes whenever anything in it does.
//
// Schedule is filtered to this agent: an agent has no business receiving,
// or being able to read, the rest of the estate's schedule.
//
// CadenceSeconds is NOT filtered, and deliberately so. It is used to judge
// whether a TARGET is behind, and a target lives on a different host from
// the source whose schedule defines its cadence -- so every agent needs the
// whole estate's cadences to assess the targets it holds. It is derived
// from the schedules rather than configured separately, which keeps one
// source of truth for "how often should this sync".
func (s *Store) AgentConfigFor(agentID string) (AgentConfig, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	set, err := s.loadSettings()
	if err != nil {
		return AgentConfig{}, "", err
	}
	sched, err := s.loadSchedules()
	if err != nil {
		return AgentConfig{}, "", err
	}

	// Operations ride in the same document, so they participate in the ETag
	// below and reach the agent on its next long poll -- seconds, not the
	// report interval. That is the whole delivery mechanism: publishing an
	// operation IS issuing it, and removing it IS acknowledging it.
	pending, err := s.pendingOperationsFor(agentID)
	if err != nil {
		return AgentConfig{}, "", err
	}

	cfg := AgentConfig{
		ReportIntervalSeconds:  set.ReportIntervalSeconds,
		PollWaitSeconds:        set.PollWaitSeconds,
		MaxConcurrentSyncs:     set.MaxConcurrentSyncs,
		TargetReplicationSlots: set.TargetReplicationSlots,
		ShutdownTimeoutSec:     set.ShutdownTimeoutSec,
		Schedule:               sched[agentID],
		Templates:              set.Templates,
		Operations:             pending,
	}

	cadence := map[string]int{}
	for _, entries := range sched {
		for _, e := range entries {
			if e.Enabled && e.IntervalSeconds > 0 {
				cadence[e.VM] = e.IntervalSeconds
			}
		}
	}
	if len(cadence) > 0 {
		cfg.CadenceSeconds = cadence
	}

	etag, err := etagOf(cfg)
	if err != nil {
		return AgentConfig{}, "", err
	}
	return cfg, etag, nil
}

// --- audit ----------------------------------------------------------------

// AppendAudit records an intent and returns its entry ID, which
// CompleteAudit uses to record what actually happened.
func (s *Store) AppendAudit(actor, action, target, detail string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []AuditEntry
	if _, err := readJSON(s.path("audit.json"), &entries); err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	entries = append(entries, AuditEntry{
		ID:     id,
		AtUnix: time.Now().Unix(),
		Actor:  actor,
		Action: action,
		Target: target,
		Detail: detail,
	})
	if err := writeJSONAtomic(s.path("audit.json"), entries, 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// CompleteAudit records the outcome of a previously-recorded intent.
func (s *Store) CompleteAudit(id, outcome string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completeAuditLocked(id, outcome)
}

// completeAuditLocked is CompleteAudit for callers already holding the lock.
func (s *Store) completeAuditLocked(id, outcome string) error {
	var entries []AuditEntry
	if _, err := readJSON(s.path("audit.json"), &entries); err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Outcome = outcome
			entries[i].DoneUnix = time.Now().Unix()
		}
	}
	return writeJSONAtomic(s.path("audit.json"), entries, 0o600)
}

func (s *Store) Audit() ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []AuditEntry
	_, err := readJSON(s.path("audit.json"), &entries)
	// Newest first: an audit log is read to answer "what just happened".
	sort.Slice(entries, func(i, j int) bool { return entries[i].AtUnix > entries[j].AtUnix })
	return entries, err
}

// --- helpers --------------------------------------------------------------

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// etagOf derives a stable tag from a value's JSON encoding. Content-based
// rather than a counter so it survives a restart: an agent long-polling
// across a UI restart must not be told its cached config changed when it
// did not.
func etagOf(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`, nil
}

func readJSON(path string, into any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// writeJSONAtomic writes via a temp file and a rename, fsyncing first.
// Without the fsync the rename can be durable while the contents are not,
// leaving a valid-looking but empty file after a power loss.
func writeJSONAtomic(path string, value any, perm os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	// Flush the DIRECTORY too, so the rename itself is durable.
	//
	// Syncing the temp file makes its contents survive a power loss; it says
	// nothing about the directory entry pointing at them. Without this the
	// rename can be lost while the data is intact and the PREVIOUS file
	// reappears -- here that means an operation the console already issued
	// comes back pending, or a schedule change silently reverts.
	//
	// The result is ignored on purpose. By this point the write has succeeded;
	// a refused directory fsync leaves only its durability unconfirmed, which
	// is where this stood before. POSIX permits the refusal and platforms
	// disagree on which error they give for it, so failing the write here
	// would trade an outage for a guarantee that cannot be had.
	_ = syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename into it is durable. Its error is
// returned for testability; writeJSONAtomic deliberately cannot act on one.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to flush it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", dir, err)
	}
	return nil
}

// ReportDisk mirrors the agent's own type: one disk file as it sits on a
// hypervisor's storage.
//
// AllocatedBytes, not ApparentBytes, is what a copy costs. A sparse qcow2
// routinely reports 500 GB apparent against 40 GB allocated, and sizing an
// inversion's aside copy off the apparent figure would refuse operations
// that fit comfortably.
type ReportDisk struct {
	Path           string `json:"path"`
	ApparentBytes  int64  `json:"apparent_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Missing        bool   `json:"missing,omitempty"`
}

// ReportRestorePoint mirrors the agent's own type: one point-in-time copy of
// a replica, sitting beside its disks.
//
// Verify is the field that earns this its place in a confirmation dialog.
// "not-run" is the ORDINARY state rather than a fault -- restore points are
// taken before -verify runs, and verify is expensive enough to run on its own
// cadence -- so an operator choosing between copies during an incident needs
// to be told which of them was ever actually compared against the source.
// Showing nothing would make "never checked" and "checked and clean" look the
// same at exactly the wrong moment.
type ReportRestorePoint struct {
	Tag              string   `json:"tag"`
	TakenAtUnix      int64    `json:"taken_at_unix"`
	CheckpointAtUnix int64    `json:"checkpoint_at_unix,omitempty"`
	Checkpoint       string   `json:"checkpoint,omitempty"`
	Source           string   `json:"source,omitempty"`
	Verify           string   `json:"verify,omitempty"`
	Disks            []string `json:"disks,omitempty"`
	Incomplete       bool     `json:"incomplete,omitempty"`
}

// ReportFenced mirrors the agent's own type: one entry from its fence
// ledger, being a fence it acted on and what came of it.
type ReportFenced struct {
	FenceID string `json:"fence_id"`
	// State is the agent ledger's own: `done`, `failed`, or `running` for
	// one interrupted mid-shutdown. Anything but done means this domain may
	// still be up alongside the promoted copy.
	State string `json:"state"`
	// AtUnix is when the attempt finished, or began if it never did.
	AtUnix int64 `json:"at_unix,omitempty"`
	// PeerRef is the promoted copy that displaced this domain, and ArmedBy
	// whoever performed that promotion -- so "why is this VM off" is
	// answerable here rather than from a journal on a hypervisor.
	PeerRef string `json:"peer_ref,omitempty"`
	ArmedBy string `json:"armed_by,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Succeeded reports whether the fence actually stopped the domain.
func (f *ReportFenced) Succeeded() bool { return f != nil && f.State == OpStateDone }

// ReportFilesystem mirrors the agent's own type: the storage behind a set
// of disks, one entry per distinct directory rather than per host.
type ReportFilesystem struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
}

// AllocatedBytes totals what a reported domain occupies on disk -- the
// figure a rename-aside consumes again on the same filesystem.
func (d ReportDomain) AllocatedBytes() int64 {
	var total int64
	for _, disk := range d.Disks {
		total += disk.AllocatedBytes
	}
	return total
}

// DefaultTemplateName is the template entries inherit from when they name
// none, and the one the agent synthesises entries from. A reserved name, not a
// flag: "is there a default" is answered by the same lookup as any other
// template.
const DefaultTemplateName = "default"

// ValidateTemplate refuses a template that would misbehave on the agent.
//
// A deliberate SUBSET of what the agent checks, and the boundary is worth
// being explicit about: this console cannot import pkg/schedcal (it depends on
// nothing outside the standard library, which is why it lives in its own
// module), so it cannot validate the verify CALENDAR's grammar. What it can
// do is every structural rule, and it does, because the alternative is
// publishing a template the agent will refuse -- a template being broken is a
// hundred entries being broken, and discovering that from an agent's journal
// is a bad way to find out.
//
// A malformed calendar expression still reaches the agent, which names it in
// Complaints() on adoption and refuses to verify that VM. Loud, but later than
// here. See docs/design/scheduling.md's Open list.
func ValidateTemplate(name string, t ScheduleTemplate) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a template needs a name")
	}
	if t.IntervalSeconds <= 0 {
		return fmt.Errorf("template %q needs a sync interval: a template with no cadence has nothing to contribute, and entries inheriting it would be due on every tick", name)
	}
	if t.VerifyIntervalSeconds < 0 {
		return fmt.Errorf("template %q has a negative verify interval", name)
	}

	hasCal := t.VerifyDays != "" || t.VerifyWindow != ""
	if hasCal && t.VerifyIntervalSeconds > 0 {
		return fmt.Errorf("template %q sets both a verify interval and a verify calendar: these are two ways to write one cadence, so set one or the other", name)
	}
	// The mode requirement lands on the DEFAULT only, matching the agent. A
	// non-default template may carry the estate-wide window alone and let each
	// entry name its own mode -- which is the shape templates are most worth
	// having -- but the default synthesises entries for VMs that have none,
	// and those have no other source for a mode.
	if name == DefaultTemplateName && (hasCal || t.VerifyIntervalSeconds > 0) && t.Profile.Verify == "" {
		return fmt.Errorf("the %q template sets a verify cadence but no verify mode. It is the default, so it also has to be complete on its own: the entries it creates for VMs with no entry of their own have nothing else to supply one", DefaultTemplateName)
	}

	// The same profile check the schedule form runs. A template's profile is
	// inherited by every VM naming it, so a bad one here is a bad hundred
	// entries -- and an agent that refuses the template skips all of them.
	return ValidateProfile(t.Profile)
}

// SetTemplate creates or replaces one template, under the same lock that reads
// the settings it lives in.
//
// Read-modify-write in a handler would race a concurrent settings save and
// silently drop one of them -- the templates map and the estate settings share
// a file.
func (s *Store) SetTemplate(name string, t ScheduleTemplate) error {
	if err := ValidateTemplate(name, t); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadSettings()
	if err != nil {
		return err
	}
	if set.Templates == nil {
		set.Templates = map[string]ScheduleTemplate{}
	}
	// The map key is what an entry refers to, so the inner name is filled from
	// it rather than trusted to match. Requiring both is a chance for the two
	// to disagree, and the agent refuses a template whose name and key differ.
	t.Name = name
	set.Templates[name] = t
	return writeJSONAtomic(s.path("settings.json"), set, 0o600)
}

// DeleteTemplate removes one, refusing while any entry still names it.
//
// An entry naming a template that does not exist is returned UNCHANGED by the
// agent's resolver, so it then fails its own validation and is skipped -- a VM
// silently stops replicating. Refusing here is the cheap end of that.
func (s *Store) DeleteTemplate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sched, err := s.loadSchedules()
	if err != nil {
		return err
	}
	var users []string
	for _, entries := range sched {
		for _, e := range entries {
			if e.Template == name {
				users = append(users, e.VM)
			}
		}
	}
	if len(users) > 0 {
		sort.Strings(users)
		if len(users) > 5 {
			users = append(users[:5], "…")
		}
		return fmt.Errorf("template %q is still used by %s: point those VMs somewhere else first, or their next sync is skipped entirely",
			name, strings.Join(users, ", "))
	}

	set, err := s.loadSettings()
	if err != nil {
		return err
	}
	delete(set.Templates, name)
	return writeJSONAtomic(s.path("settings.json"), set, 0o600)
}

// ValidateEntryCadence refuses a schedule entry whose verify cadence the agent
// would reject, checked against the template it inherits from.
//
// The both-forms rule needs only the ENTRY, and that is a property of how the
// agent resolves rather than a shortcut: the verify cadence inherits as one
// unit, so an entry stating either form inherits neither half of the other. A
// resolved entry can therefore carry both only if the entry itself did.
//
// The mode rule does need the template, because either half can come from
// either place -- an entry supplying the calendar under a template supplying
// the mode is the combination templates exist to allow.
//
// The calendar's GRAMMAR is still not checked here and cannot be: it needs
// pkg/schedcal, which lives in the agent's module. An expression this accepts
// can still be refused there, which the Schedule page then shows.
func ValidateEntryCadence(e ScheduleEntry, templates map[string]ScheduleTemplate) error {
	hasCal := e.VerifyDays != "" || e.VerifyWindow != ""
	if e.VerifyIntervalSeconds < 0 {
		return fmt.Errorf("the verify interval cannot be negative")
	}
	if hasCal && e.VerifyIntervalSeconds > 0 {
		return fmt.Errorf("this VM sets both a verify interval and a verify calendar: they are two ways to write one cadence, so use one or the other")
	}
	if !hasCal && e.VerifyIntervalSeconds == 0 {
		return nil
	}

	// Which verify mode will actually be in force, by the agent's own
	// resolution: the entry's own, else the template it names, else the
	// default's.
	mode := e.Profile.Verify
	if mode == "" {
		name := e.Template
		if name == "" {
			name = DefaultTemplateName
		}
		if t, ok := templates[name]; ok {
			mode = t.Profile.Verify
		}
	}
	if mode == "" {
		return fmt.Errorf("this VM sets a verify cadence but no verify mode, and neither does the template it inherits from: the cadence says how often to verify, not whether to")
	}
	return nil
}
