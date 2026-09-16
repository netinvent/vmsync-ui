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
	"strconv"
	"strings"
	"time"

	"vmsync-ui/internal/store"
)

// ScheduleRow is one candidate VM on one agent: something this host is the
// SOURCE for, and therefore something it could be told to sync.
//
// Only sources appear. A target is synced BY the agent on its source host,
// so offering to schedule it here would produce an entry that host could
// never act on.
type ScheduleRow struct {
	AgentID  string
	Hostname string
	VM       string
	Targets  []string
	// Entry is the current schedule entry, or nil when this VM is not
	// scheduled. Nil is a meaningful state: "this source exists but nothing
	// is replicating it on a timer" is exactly what an operator is looking
	// for on this page.
	Entry      *store.ScheduleEntry
	LastResult *store.SyncResult
	// AgentStale means whatever this row shows may be out of date, and any
	// change made now will not reach the host until it checks back in.
	AgentStale bool
	// Effective is what the AGENT last reported it will actually do for this
	// VM, after resolving templates. Nil when the agent has not reported one.
	//
	// Distinct from Entry, and the gap between them is the point: Entry is
	// what this console published, Effective is what the host runs. A VM
	// covered by a default template has no Entry at all and a perfectly good
	// Effective.
	Effective *store.EffectiveScheduleEntry
	// AgentTimezone is the zone Effective's windows are expressed in -- the
	// agent host's own, displayed rather than chosen -- and AgentTZOffset is
	// that zone's offset from UTC at the moment the agent reported.
	//
	// The offset is carried rather than derived because this server does not
	// know the agent's zone: it has a name like "CEST" and no tzdata entry to
	// resolve it against, so rendering without the offset would silently use
	// the console's own clock and describe a different night.
	AgentTimezone string
	AgentTZOffset int

	// AgentReadOnly is true when the agent runs in monitor mode: it reports
	// what happened on that host and runs nothing this console publishes.
	//
	// The row still appears, with everything it observed, because observing
	// is the whole point of a monitored host -- hiding it would leave a site
	// invisible, which is the problem monitor mode exists to solve. What
	// changes is that the page must not present an interval on such a row as
	// something it will act on: a schedule saved against a monitor agent is
	// stored, shown, and never read by anything.
	AgentReadOnly bool
}

// SyncedElsewhere reports whether this VM's replication is driven by something
// outside this control plane.
//
// The name is the honest one. It is not "unscheduled" -- the host may well be
// syncing every fifteen minutes from a crontab, and the freshness this row
// shows is real. It is that nothing THIS console publishes reaches it.
func (r ScheduleRow) SyncedElsewhere() bool { return r.AgentReadOnly }

// Scheduled reports whether this VM has an entry THIS CONSOLE published.
//
// Deliberately still about the entry alone: it drives the form below, and a
// VM the agent covers from a default template has nothing here to edit.
func (r ScheduleRow) Scheduled() bool { return r.Entry != nil }

// AgentCovers reports whether the agent will actually replicate this VM,
// whether or not anybody wrote it an entry.
//
// The two can disagree, and only in one direction: a default template covers
// VMs with no entry. Counting those as "unscheduled" -- which this page did
// until the agent started reporting its effective schedule -- tells an
// operator to go and schedule a VM that is already being replicated every
// fifteen minutes.
func (r ScheduleRow) AgentCovers() bool {
	return r.Effective != nil && r.Effective.Enabled
}

// CoveredByDefault is a VM the agent replicates that nobody scheduled here.
func (r ScheduleRow) CoveredByDefault() bool {
	return r.Entry == nil && r.AgentCovers() && r.Effective.Synthesised
}

// IntervalMinutes renders the interval for a form field.
func (r ScheduleRow) IntervalMinutes() int {
	if r.Entry == nil || r.Entry.IntervalSeconds <= 0 {
		return 15
	}
	return r.Entry.IntervalSeconds / 60
}

// VerifyIntervalMinutes renders the verify cadence for the form, and returns
// a string rather than an int because **blank is a meaningful value**: it
// means "verify on every sync", which is what a profile naming a verify mode
// did before this field existed and still does. Rendering 0 into a number
// input would put a literal "0" in the box, which is neither valid (min=1)
// nor what blank means.
func (r ScheduleRow) VerifyIntervalMinutes() string {
	if r.Entry == nil || r.Entry.VerifyIntervalSeconds <= 0 {
		return ""
	}
	return strconv.Itoa(r.Entry.VerifyIntervalSeconds / 60)
}

// SelectedPreset is the preset to preselect, defaulting to the LAN profile
// rather than the most aggressive one: a wrong guess toward less
// compression costs bandwidth, while a wrong guess toward more costs CPU on
// a production hypervisor.
func (r ScheduleRow) SelectedPreset() string {
	if r.Entry != nil && r.Entry.Preset != "" {
		return r.Entry.Preset
	}
	return "lan"
}

func (r ScheduleRow) Verify() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.Profile.Verify
}

// VerifyFailureReinit preselects what an unscheduled VM's form offers for
// "on verify failure", and false is the honest default: it matches vmsync's
// own, so the form shows what would actually happen rather than what this UI
// would prefer. An operator turning it on is choosing it.
func (r ScheduleRow) VerifyFailureReinit() bool {
	if r.Entry == nil {
		return false
	}
	return r.Entry.Profile.VerifyFailureReinit
}

func (r ScheduleRow) TargetDiskPath() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.Profile.TargetDiskPath
}

func (r ScheduleRow) Retention() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.Profile.Retention
}

func (r ScheduleRow) TimestampToleranceSec() int {
	if r.Entry == nil {
		return 0
	}
	return r.Entry.Profile.TimestampToleranceSec
}

func (r ScheduleRow) TargetHost() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.TargetHost
}

// ShutdownTimeoutSec renders the per-VM override for a form field, empty
// when there is none.
//
// Empty rather than 0 on purpose: the field's placeholder shows the estate
// default, so a blank box reads as "inherits that" -- which is what it does.
// A literal 0 would look like a setting somebody chose, and a shutdown
// timeout of zero is not a thing anybody would choose.
func (r ScheduleRow) ShutdownTimeoutSec() string {
	if r.Entry == nil || r.Entry.ShutdownTimeoutSec <= 0 {
		return ""
	}
	return strconv.Itoa(r.Entry.ShutdownTimeoutSec)
}

// ScheduleView is everything the schedule page renders.
type ScheduleView struct {
	Rows     []ScheduleRow
	Presets  []store.Preset
	Settings store.Settings
	// Unscheduled counts sources NOTHING will replicate -- neither an entry
	// published here nor an agent-side default template.
	//
	// It used to count sources with no entry, which over-reported the moment
	// default templates existed: a VM the agent syncs every fifteen minutes
	// appeared in the number that says "these need a decision from you".
	Unscheduled int
	// TemplateNames is what the per-VM selector offers, sorted. Empty when no
	// template exists, in which case the form hides the control rather than
	// showing an empty dropdown.
	TemplateNames []string
}

// BuildScheduleView correlates agents, their reports and the stored
// schedules into one editable list.
func BuildScheduleView(
	agents []store.Agent,
	reports map[string]store.Report,
	schedules map[string][]store.ScheduleEntry,
	settings store.Settings,
	now time.Time,
) ScheduleView {
	v := ScheduleView{Presets: store.Presets, Settings: settings}
	for name := range settings.Templates {
		v.TemplateNames = append(v.TemplateNames, name)
	}
	sort.Strings(v.TemplateNames)

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

		byVM := map[string]store.ScheduleEntry{}
		for _, e := range schedules[a.ID] {
			byVM[e.VM] = e
		}
		latest := latestResults(rep.Syncs)

		// What this host says it will actually do, keyed by VM. Absent from an
		// agent too old to send it, or from one that has never run a
		// scheduler -- in which case the rows simply fall back to showing the
		// published entry, which is what this page did before.
		effective := map[string]store.EffectiveScheduleEntry{}
		for _, e := range rep.EffectiveSchedule {
			effective[e.VM] = e
		}
		agentTZ := rep.Timezone
		agentTZOffset := rep.TimezoneOffsetSeconds

		for _, dom := range rep.Domains {
			if len(dom.ReplicaTargets) == 0 {
				continue
			}
			row := ScheduleRow{
				AgentID:       a.ID,
				Hostname:      host,
				VM:            dom.Name,
				Targets:       dom.ReplicaTargets,
				AgentStale:    stale,
				AgentReadOnly: a.ReadOnly(),
			}
			if e, ok := byVM[dom.Name]; ok {
				entry := e
				row.Entry = &entry
			}
			if eff, ok := effective[dom.Name]; ok {
				e := eff
				row.Effective = &e
				row.AgentTimezone = agentTZ
				row.AgentTZOffset = agentTZOffset
			}
			// Counted on what the AGENT does, not on what was published here.
			// A VM a default template covers needs no decision from anybody,
			// and listing it as needing one is how an operator ends up writing
			// a redundant entry for a VM already syncing every fifteen minutes.
			//
			// A monitor agent's VMs are the same mistake seen further along:
			// nothing this console publishes reaches that host, so counting
			// them here would send an operator to write entries that will be
			// stored, displayed, and never read -- while the VM was being
			// synced by cron the whole time. The row still shows, and shows
			// how fresh the replica actually is; it just does not ask for a
			// decision this console cannot carry out.
			if !row.Scheduled() && !row.AgentCovers() && !row.SyncedElsewhere() {
				v.Unscheduled++
			}
			if res, ok := latest[dom.Name]; ok {
				r := res
				row.LastResult = &r
			}
			v.Rows = append(v.Rows, row)
		}
	}

	// Unscheduled first, then by host and VM: the rows that need a decision
	// are the reason to open this page.
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].Scheduled() != v.Rows[j].Scheduled() {
			return !v.Rows[i].Scheduled()
		}
		if v.Rows[i].Hostname != v.Rows[j].Hostname {
			return v.Rows[i].Hostname < v.Rows[j].Hostname
		}
		return v.Rows[i].VM < v.Rows[j].VM
	})
	return v
}

// latestResults keeps only the most recent outcome per VM.
func latestResults(results []store.SyncResult) map[string]store.SyncResult {
	out := map[string]store.SyncResult{}
	for _, r := range results {
		if prev, ok := out[r.VM]; !ok || r.StartedAtUnix > prev.StartedAtUnix {
			out[r.VM] = r
		}
	}
	return out
}

// RunView is one row of the recent-activity list.
type RunView struct {
	Host       string
	VM         string
	TargetHost string
	When       string
	Duration   string
	OK         bool
	// Unknown is a run the agent never saw the end of -- an adopted one,
	// started by a previous instance of that agent. Separate from OK rather
	// than folded into it, because a template with only ok/failed has to put
	// such a run in one of those buckets and both are wrong.
	Unknown bool
	// Busy is vmsync standing down on lock contention without touching
	// anything. Not a failure, and not a sync either.
	Busy bool
	// Degraded is a run that WORKED but left something needing a person --
	// most importantly a source guest whose filesystems are still frozen.
	// Rendered distinctly from ok, because an unqualified green tick is how
	// that stayed invisible.
	Degraded bool
	Detail   string
	// StartedAtUnix is what the list is ordered by. The humanised When is
	// for reading only — sorting on it would order "10m ago" before
	// "2m ago", which is the wrong way round and quietly so.
	StartedAtUnix int64
}

// BuildRunView flattens every agent's recent sync results, newest first.
func BuildRunView(agents []store.Agent, reports map[string]store.Report, now time.Time, limit int) []RunView {
	var out []RunView
	for _, a := range agents {
		rep, ok := reports[a.ID]
		if !ok {
			continue
		}
		host := rep.Hostname
		if host == "" {
			host = a.Hostname
		}
		for _, r := range rep.Syncs {
			rv := RunView{
				Host:          host,
				VM:            r.VM,
				TargetHost:    r.TargetHost,
				When:          humanAge(now.Sub(time.Unix(r.StartedAtUnix, 0))) + " ago",
				Duration:      humanAge(time.Duration(r.DurationSecs) * time.Second),
				OK:            r.Succeeded(),
				Unknown:       r.Unobserved(),
				Busy:          r.Outcome == store.OutcomeBusy,
				Degraded:      r.Degraded,
				StartedAtUnix: r.StartedAtUnix,
			}
			switch {
			case rv.Unknown:
				// Not a failure, and there is no error text to show: nobody
				// observed how it ended. Say that, rather than leaving a row
				// whose blank detail reads like "nothing went wrong".
				rv.Detail = "this agent restarted while the sync was running, so its outcome was never observed"
				if r.RunID != "" {
					rv.Detail += " (run " + r.RunID + ")"
				}
			case rv.Busy:
				rv.Detail = "stood down: another vmsync was already working on this domain"
			case !rv.OK:
				// The failure reason belongs on the row. Sending an operator
				// to a hypervisor's journal to find out why is the whole
				// thing this page exists to avoid.
				rv.Detail = strings.TrimSpace(r.Error)
				if tail := strings.TrimSpace(lastLine(r.LogTail)); tail != "" {
					rv.Detail = tail
				}
			}
			// PREPENDED, outside the switch, and never dropped for whichever
			// branch ran. A degradation is orthogonal to the outcome -- an
			// adopted run can be unobserved AND have left a guest frozen --
			// and of the two, the frozen guest is the one still happening.
			if reason := strings.TrimSpace(r.DegradedReason); rv.Degraded && reason != "" {
				if rv.Detail != "" {
					rv.Detail = reason + " — " + rv.Detail
				} else {
					rv.Detail = reason
				}
			}
			out = append(out, rv)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAtUnix > out[j].StartedAtUnix })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// VerifyCadence is the resolved verify schedule in one short phrase, or "" when
// the agent has not reported one.
//
// Reads from Effective, never from Entry: the entry may say nothing at all and
// still inherit a monthly window from its template, and showing the blank is
// how this page came to describe a schedule that was never the schedule.
func (r ScheduleRow) VerifyCadence() string {
	if r.Effective == nil || r.Effective.VerifyMode == "" {
		return ""
	}
	e := r.Effective
	switch {
	case e.CalendarError != "":
		return e.VerifyMode + " — calendar invalid"
	case e.VerifyDays != "" && e.VerifyWindow != "":
		return e.VerifyMode + " on " + e.VerifyDays + ", " + e.VerifyWindow
	case e.VerifyDays != "":
		return e.VerifyMode + " on " + e.VerifyDays
	case e.VerifyWindow != "":
		return e.VerifyMode + " daily, " + e.VerifyWindow
	case e.VerifyIntervalSeconds > 0:
		return e.VerifyMode + " every " + shortDuration(time.Duration(e.VerifyIntervalSeconds)*time.Second)
	default:
		return e.VerifyMode + " every sync"
	}
}

// NextVerify is when the verify window next opens, in the AGENT's timezone,
// or "" when there is no calendar.
//
// Rendered from the agent's own offset rather than this server's clock. The
// window means the quiet hours where the disks are, and a console in another
// zone showing its own local time would be describing a different night.
func (r ScheduleRow) NextVerify() string {
	if r.Effective == nil || r.Effective.NextVerifyUnix == 0 {
		return ""
	}
	if r.Effective.VerifyWindowOpen {
		return "open now"
	}
	zone := time.FixedZone(r.AgentTimezone, r.AgentTZOffset)
	return time.Unix(r.Effective.NextVerifyUnix, 0).In(zone).Format("Mon 2 Jan 15:04 MST")
}

// VerifyProblem is the reason this VM will never verify, when there is one.
func (r ScheduleRow) VerifyProblem() string {
	if r.Effective == nil {
		return ""
	}
	return r.Effective.CalendarError
}

// shortDuration renders a cadence the way an operator says it out loud.
func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// EffectiveIntervalMinutes is the resolved sync cadence, for the read-only
// view of a VM this console published no entry for.
func (r ScheduleRow) EffectiveIntervalMinutes() int {
	if r.Effective == nil || r.Effective.IntervalSeconds <= 0 {
		return 0
	}
	return r.Effective.IntervalSeconds / 60
}

// SelectedTemplate is the template this VM's entry names, for the form's
// selector. Empty means none, which is a real choice rather than a blank.
func (r ScheduleRow) SelectedTemplate() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.Template
}

// VerifyDays and VerifyWindow render the calendar form of the verify cadence
// for the form. Blank means "inherit from the template", which is a real
// value rather than a placeholder.
func (r ScheduleRow) VerifyDays() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.VerifyDays
}

func (r ScheduleRow) VerifyWindow() string {
	if r.Entry == nil {
		return ""
	}
	return r.Entry.VerifyWindow
}
