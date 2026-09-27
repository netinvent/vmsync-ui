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
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"vmsync-ui/internal/auth"
	"vmsync-ui/internal/store"
)

var now = time.Unix(1_800_000_000, 0)

func testServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	hash, err := auth.HashPassword("a-sufficiently-long-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	am, err := auth.NewManager([]auth.Account{{Username: "op", PasswordHash: hash, Role: auth.RoleAdmin}}, time.Hour)
	if err != nil {
		t.Fatalf("auth manager: %v", err)
	}
	s, err := New(st, am, slog.New(slog.NewTextHandler(io.Discard, nil)), true, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// fullFixture is deliberately rich enough that every table body on every
// page has at least one row: a fixture that renders only empty states
// makes the render test below pass while proving almost nothing.
func fullFixture() ([]store.Agent, map[string]store.Report, map[string][]store.ScheduleEntry) {
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", AgentVersion: "0.30", LastSeenAt: now.Unix() - 30},
		{ID: "tgt", Hostname: "hyper02p", AgentVersion: "0.30", LastSeenAt: now.Unix() - 30},
	}
	reports := map[string]store.Report{
		"src": {
			Hostname: "hyper01p",
			Domains: []store.ReportDomain{
				// Scheduled, and one target: the form hides the target-host field.
				{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok", Active: true},
				// Unscheduled, several targets: nil Entry plus the required
				// target-host field. Both are branches the template takes.
				{Name: "db01", ReplicaTargets: []string{"hyper02p:db01", "hyper03p:db01"}, Status: "ok", Active: true},
				{Name: "scratch01", Status: "unreplicated", Active: true},
			},
			Syncs: []store.SyncResult{
				{VM: "web01", TargetHost: "hyper02p", StartedAtUnix: now.Unix() - 300, DurationSecs: 42, ExitCode: exit(0)},
				{VM: "db01", TargetHost: "hyper02p", StartedAtUnix: now.Unix() - 120, DurationSecs: 9,
					ExitCode: exit(1), Error: "exit status 1", LogTail: "connecting\nfatal: no route to host\n"},
			},
		},
		"tgt": {
			Hostname: "hyper02p",
			Domains: []store.ReportDomain{
				{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 300},
				// Carries a recorded verification failure as well as a
				// critical status, so the branch that flags a copy known
				// not to match is taken by the render test below.
				{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "critical", AgeSeconds: -1,
					VerifyState: "failed", VerifyFailedAtUnix: now.Unix() - 7200,
					Reasons: []string{"no successful sync has ever completed against this target"}},
			},
		},
	}
	schedules := map[string][]store.ScheduleEntry{"src": {{
		VM: "web01", IntervalSeconds: 900, Enabled: true, Preset: "wan", TargetHost: "hyper02p",
		Profile: store.SyncProfile{Verify: "full", TargetDiskPath: "/data/replicas"},
	}}}
	return agents, reports, schedules
}

// TestEveryTemplateRenders is the test html/template most needs: a typo in
// a field name or an action is a runtime error, not a compile error, so
// without this the first time a page is exercised is in front of an
// operator during an incident.
func TestEveryTemplateRenders(t *testing.T) {
	s := testServer(t)
	user := auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"}

	agents, reports, schedules := fullFixture()
	// The failover page is fed its own fixture: fullFixture has no promoted
	// domain, so every action branch on that page would go untaken and the
	// loop below would prove only that the table header renders.
	foAgents, foReports := failoverFixture()
	failover := BuildFailoverView(foAgents, foReports, failoverOperationFixture(), now)
	data := pageData{
		User:      user,
		Active:    "dashboard",
		Dashboard: BuildDashboard(agents, reports, now),
		Runs:      BuildRunView(agents, reports, now, 15),
		Agents:    BuildDashboard(agents, reports, now).Agents,
		Audit:     []store.AuditEntry{{ID: "e1", AtUnix: now.Unix(), Actor: "op", Action: "revoke-agent", Target: "a1"}},
		Schedule:  BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now),
		Failover:  failover,
		NewToken:  "abc123",
		Flash:     "done",
	}
	if len(data.Failover.Rows) == 0 || len(data.Failover.Operations) == 0 || data.Failover.SplitBrainCount == 0 {
		// Same reasoning as the guard below, for the page whose branches
		// only appear when something has actually gone wrong.
		t.Fatal("failover fixture produced no rows, no operations, or no split brain, so those branches would never be rendered")
	}
	if len(data.Schedule.Rows) == 0 || len(data.Runs) == 0 {
		// Without this the loop below proves only that the empty-state
		// branches render, which is not what these pages are for.
		t.Fatal("fixture produced no schedule rows or runs, so the tables below would never be rendered")
	}

	for _, name := range []string{"dashboard.html", "agents.html", "audit.html", "login.html", "schedule.html", "failover.html"} {
		t.Run(name, func(t *testing.T) {
			var buf strings.Builder
			if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
				t.Fatalf("rendering %s: %v", name, err)
			}
			if buf.Len() == 0 {
				t.Fatalf("%s rendered nothing", name)
			}
		})
	}
}

func TestDashboardSurfacesWhatMatters(t *testing.T) {
	s := testServer(t)
	agents := []store.Agent{{ID: "a1", Hostname: "hyper01p", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {
		Hostname: "hyper01p",
		Domains: []store.ReportDomain{
			{Name: "web01", ReplicaSource: "hyper00p:web01", Status: "critical", AgeSeconds: -1,
				Reasons: []string{"no successful sync has ever completed"}},
			{Name: "scratch01", Status: "unreplicated"},
		},
	}}

	var buf strings.Builder
	err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: BuildDashboard(agents, reports, now),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// An unprotected VM must be impossible to miss -- it is the failure a
	// green dashboard hides by simply not listing it.
	if !strings.Contains(html, "Not replicated") || !strings.Contains(html, "scratch01") {
		t.Error("the unprotected VM is not called out on the page")
	}
	// The reason belongs on the row, not behind a click.
	if !strings.Contains(html, "no successful sync has ever completed") {
		t.Error("the reason for a critical pair is not shown inline")
	}
	// A source nobody reports for is a blind spot, not a healthy pair.
	if !strings.Contains(html, "no agent reporting this source") {
		t.Error("an unreported source host is not flagged")
	}
}

// A replica whose contents were compared against its source and found to
// differ must say so on its own row, and must not be readable as healthy.
//
// Nothing else on this page asks that question. Status and the behind column
// answer how far BEHIND a copy is, and the status word arrives from the agent:
// one too old to assess the finding reports "ok", and a current one reports
// "critical", which is also what a replica that is merely far behind reports.
// Either way the row alone would not tell an operator that the copy is WRONG
// -- which is exactly the row that gets promoted during an incident without a
// second thought. This test fixes the domain's reported status at "ok" on
// purpose, so it proves the console flags the finding on its own evidence
// rather than by echoing the agent's verdict.
func TestDashboardFlagsAReplicaKnownNotToMatchItsSource(t *testing.T) {
	s := testServer(t)
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaTargets: []string{"hyper02p:db01"}, Status: "ok"},
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			// Current by every measure this page had before: an ok status,
			// two minutes behind, no failed attempts -- and wrong.
			{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "ok", AgeSeconds: 120,
				VerifyState: "failed", VerifyFailedAtUnix: now.Unix() - 7200},
			{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	var bad, clean Pair
	for _, p := range d.Pairs {
		switch p.TargetVM {
		case "db01":
			bad = p
		case "web01":
			clean = p
		}
	}
	if !bad.VerifyFailed() {
		t.Fatal("the pair does not carry the target's recorded verification failure, so nothing downstream can render it")
	}
	if bad.VerifyFailedAt() == "" {
		t.Error("the date of the failure is not exposed; a bare marker cannot tell last night from last month")
	}
	if clean.VerifyFailed() {
		t.Error("a replica with no recorded failure reports one -- an alarm on every row is one nobody reads")
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "verify failed") {
		t.Error("the row carries no visible marker, so a copy known to be wrong reads as healthy")
	}
	if !strings.Contains(html, bad.VerifyFailedAt()) {
		t.Errorf("the date %q is not on the page", bad.VerifyFailedAt())
	}
	if !strings.Contains(html, "differing from its source") {
		t.Error("the marker is not explained; a word nobody can decode during an incident is not a warning")
	}
	// Both pairs go through the same template, so a marker rendered
	// unconditionally would flag the healthy one too -- and a page that
	// shouts about every row says nothing about any of them.
	if n := strings.Count(html, "verify failed"); n != 1 {
		t.Errorf("the marker appears %d times for one failing replica out of two", n)
	}
}

// The board's hardest case: a replica whose rebuild was interrupted.
//
// Everything the dashboard normally judges a pair by is, on such a row,
// truthful about a copy that is no longer on disk. The interrupted run
// renamed the good disks aside and began writing new ones, and it never
// updated the metadata -- so the checkpoint, the lag and the failure count
// still describe the copy it displaced. The pair therefore renders as a
// perfectly ordinary healthy one, which is exactly how a half-written
// replica went unnoticed until somebody promoted it.
//
// The clean pair beside it is not decoration. A marker rendered
// unconditionally would flag the whole estate, and a board that shouts about
// every row says nothing about any of them.
func TestDashboardFlagsAReplicaLeftHalfWrittenByAnInterruptedRebuild(t *testing.T) {
	s := testServer(t)
	const raw = "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hyper02p,aside=1758441600"
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaTargets: []string{"hyper02p:db01"}, Status: "ok"},
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			// Healthy by every column this page has: ok, two minutes
			// behind, no failed attempts, a checkpoint -- and its disks are
			// a partial copy.
			{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "ok", AgeSeconds: 120,
				LastCheckpoint: "vmsync-1758441000", ReplicaIncomplete: raw},
			{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	var bad, clean Pair
	for _, p := range d.Pairs {
		switch p.TargetVM {
		case "db01":
			bad = p
		case "web01":
			clean = p
		}
	}
	if !bad.ReplicaPartial() {
		t.Fatal("the pair does not carry the target's interrupted-rebuild marker, so nothing downstream can render it")
	}
	if bad.ReplicaPartialUnreadable() {
		t.Error("a value in the engine's own grammar read as unreadable")
	}
	if bad.ReplicaPartialVerb() != "reinit" {
		t.Errorf("ReplicaPartialVerb() = %q, want reinit", bad.ReplicaPartialVerb())
	}
	if bad.ReplicaPartialAt() == "" {
		t.Error("the date of the interrupted rebuild is not exposed; a bare marker cannot tell last night from last month")
	}
	if bad.ReplicaPartialHost() != "hyper02p" {
		t.Errorf("ReplicaPartialHost() = %q, want hyper02p", bad.ReplicaPartialHost())
	}
	if bad.ReplicaPartialAside() != ".vmsync-replaced-1758441600" {
		t.Errorf("ReplicaPartialAside() = %q, want the suffix the complete copy was renamed with",
			bad.ReplicaPartialAside())
	}
	if clean.ReplicaPartial() {
		t.Error("a replica with no marker reports one -- an alarm on every row is one nobody reads")
	}
	// The trap, restated as an assertion: this row still looks fine by every
	// measure the board had before. If that ever stops being true the test
	// has stopped covering the case it was written for.
	if bad.Status != "ok" || bad.FailureCount != 0 {
		t.Fatalf("the fixture no longer models a row that reads healthy: status=%q failures=%d",
			bad.Status, bad.FailureCount)
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		// The marker itself, beside the status rather than instead of it.
		"partial copy",
		// The dated explanation, and the two facts that are not guessable
		// from anything else on the row.
		bad.ReplicaPartialAt(),
		"never recorded finishing",
		"describes a different copy",
		".vmsync-replaced-1758441600",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the rendered board is missing %q, so a half-written replica reads as healthy", want)
		}
	}
	// The status word survives alongside the pill. Replacing it would trade
	// one true fact for another instead of showing both.
	if !strings.Contains(html, `<span class="pill s-ok">ok</span>`) {
		t.Error("the marker replaced the status rather than sitting beside it")
	}
	// Two pairs go through one template, and only one of them is affected.
	if n := strings.Count(html, "PARTIAL COPY"); n != 1 {
		t.Errorf("the explanation appears %d times for one affected replica out of two", n)
	}
}

// An unreadable value is the case that decides whether this marker can be
// trusted at all. It must warn anyway, show the operator the exact text the
// agent reported, and say plainly that the detail could not be read -- the
// alternative, dropping a row whose value came from a newer engine, is a
// silent green board over a replica nobody can promote.
func TestDashboardStillWarnsWhenThePartialCopyValueCannotBeRead(t *testing.T) {
	s := testServer(t)
	const raw = "verb=rebase-overlay,at=1758441600,host=hyper02p"
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaTargets: []string{"hyper02p:db01"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "ok", AgeSeconds: 120,
				ReplicaIncomplete: raw},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	p := d.Pairs[0]
	if !p.ReplicaPartial() {
		t.Fatal("a value this build cannot parse dropped the marker, which is the one direction it must never fail in")
	}
	if !p.ReplicaPartialUnreadable() {
		t.Fatal("an unknown verb was reported as understood, so the page would explain a run it knows nothing about")
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		"partial copy",
		"PARTIAL COPY",
		"could not read the detail",
		// Verbatim, so an operator can act on evidence this build could not
		// interpret.
		raw,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the rendered board is missing %q for an unreadable marker", want)
		}
	}
}

// dashboardPartialCopyFixture is one pair whose replica carries the given
// marker beside an identical pair that carries none.
//
// The clean pair is load-bearing rather than padding: both go through the
// same template, so it is what proves anything the affected row prints is
// conditional. A detail rendered on every row is a detail nobody reads.
func dashboardPartialCopyFixture(raw string) ([]store.Agent, map[string]store.Report) {
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaTargets: []string{"hyper02p:db01"}, Status: "ok"},
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "ok", AgeSeconds: 120,
				LastCheckpoint: "vmsync-1758441000", ReplicaIncomplete: raw},
			{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120},
		}},
	}
	return agents, reports
}

// The correlation id is the one part of this marker that leads anywhere off
// the row, and until now it was parsed and then shown nowhere.
//
// Everything else the explanation prints describes the wreck; this is the
// string that joins the row to the console's own audit entry for the run --
// who started it and when -- and to the journal the engine wrote beside the
// disks. The README documents it as the id to grep the hypervisor with,
// which it cannot be while the console keeps it to itself. A value parsed,
// stored and never rendered is the same shape of defect as a verdict nothing
// reads.
func TestTheDashboardPrintsTheActionIdOfAnInterruptedRebuild(t *testing.T) {
	s := testServer(t)
	const raw = "verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hyper02p,aside=1758441600"
	agents, reports := dashboardPartialCopyFixture(raw)

	d := BuildDashboard(agents, reports, now)
	var bad, clean Pair
	for _, p := range d.Pairs {
		switch p.TargetVM {
		case "db01":
			bad = p
		case "web01":
			clean = p
		}
	}
	if bad.ReplicaPartialAction() != "9f3c1a2b4d5e6f70" {
		t.Fatalf("ReplicaPartialAction() = %q, want the id the engine stamped the run with -- "+
			"without it the marker names no record anyone can go and read", bad.ReplicaPartialAction())
	}
	if clean.ReplicaPartialAction() != "" {
		t.Errorf("a replica with no marker reports the action id %q", clean.ReplicaPartialAction())
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// Rendered, and rendered as something meant to be copied: this id is
	// typed into a grep on the hypervisor, so it may not arrive reflowed into
	// prose.
	if !strings.Contains(html, "<code>9f3c1a2b4d5e6f70</code>") {
		t.Error("the board never prints the correlation id, so the audit entry and the hypervisor's " +
			"journal for this rebuild cannot be found from the page that reports it")
	}
	// And said to be for something. A bare hex string beside a warning is
	// noise; the words are what make it actionable.
	if !strings.Contains(html, "audit log") {
		t.Error("the id is printed with nothing saying what to do with it")
	}
	// Once, for the one affected replica out of two.
	if n := strings.Count(html, "9f3c1a2b4d5e6f70"); n != 1 {
		t.Errorf("the correlation id appears %d times for one affected replica out of two", n)
	}
}

// A marker written by an engine that predates the field carries no action=,
// and that costs the trail and nothing else.
//
// The failure this guards against is the lazy fix: printing the id
// unconditionally, which leaves an empty <code></code> sitting in the warning
// of every replica whose rebuild died under an older engine. That reads as a
// value that should be there and is missing, which is worse than saying
// nothing -- an operator would go looking for a record that was never
// written.
func TestTheDashboardPrintsNoActionIdWhenTheMarkerCarriesNone(t *testing.T) {
	s := testServer(t)
	const raw = "verb=reinit,at=1758441600,host=hyper02p,aside=1758441600"
	agents, reports := dashboardPartialCopyFixture(raw)

	d := BuildDashboard(agents, reports, now)
	var bad Pair
	for _, p := range d.Pairs {
		if p.TargetVM == "db01" {
			bad = p
		}
	}
	if !bad.ReplicaPartial() || bad.ReplicaPartialUnreadable() {
		t.Fatalf("the fixture no longer models a readable marker: partial=%v unreadable=%v",
			bad.ReplicaPartial(), bad.ReplicaPartialUnreadable())
	}
	if bad.ReplicaPartialAction() != "" {
		t.Fatalf("ReplicaPartialAction() = %q for a value carrying no action=", bad.ReplicaPartialAction())
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "<code></code>") {
		t.Error("an empty code element reached the page, which reads as a correlation id that went missing " +
			"rather than one that was never written")
	}
	// The rest of the warning is untouched by the absence: the row is still
	// a replica nobody may promote, and that never depended on the id.
	if !strings.Contains(html, "PARTIAL COPY") {
		t.Error("a marker carrying no action id lost the explanation entirely")
	}
}

func TestBuildDashboardPairsComeFromTheTarget(t *testing.T) {
	// vmsync writes last_sync and failure_count onto the TARGET, so a pair's
	// freshness lives there. Building rows from the source would report
	// every source in the estate as permanently stale.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 120, FailureCount: 0},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.Pairs) != 1 {
		t.Fatalf("got %d pairs, want exactly 1 -- the source and target must collapse into one row", len(d.Pairs))
	}
	p := d.Pairs[0]
	if p.SourceHost != "hyper01p" || p.TargetHost != "hyper02p" || p.AgeSeconds != 120 {
		t.Errorf("pair = %+v, want the target's own freshness with both ends named", p)
	}
	if !p.SourceSeen {
		t.Error("SourceSeen is false although an agent reported the source domain")
	}
	if len(d.MissingAgents) != 0 {
		t.Errorf("MissingAgents = %v, want none -- both hosts have agents", d.MissingAgents)
	}
}

func TestBuildDashboardFlagsHostsWithNoAgent(t *testing.T) {
	agents := []store.Agent{{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
		{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 60},
	}}}

	d := BuildDashboard(agents, reports, now)
	if len(d.MissingAgents) != 1 || d.MissingAgents[0] != "hyper01p" {
		t.Errorf("MissingAgents = %v, want [hyper01p] -- a referenced host nobody reports for is a blind spot", d.MissingAgents)
	}
	if d.Pairs[0].SourceSeen {
		t.Error("SourceSeen should be false when no agent reported that source")
	}
}

func TestBuildDashboardFlagsSourceWithNoMatchingTarget(t *testing.T) {
	// hyper02p reports, but the replica haproxy01p's metadata names is not
	// in its report: deleted, renamed or never created. No pair row can be
	// built from the target side, so without this the source is invisible
	// on the availability page -- replicating nowhere, silently.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "haproxy01p.domain.local", ReplicaTargets: []string{"hyper02p:haproxy01p.domain.local"}, Status: "ok", Active: true},
		}},
		"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "hap01l.test.local", ReplicaSource: "hyper01p:hap01l.test.local", Status: "ok", AgeSeconds: 60},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1 -- only the target hyper02p actually reported builds a row", len(d.Pairs))
	}
	if len(d.MissingAgents) != 0 {
		t.Fatalf("MissingAgents = %v, want none -- hyper02p has an agent; its replica is what is missing", d.MissingAgents)
	}
	if len(d.MissingTargets) != 1 {
		t.Fatalf("got %d missing targets, want exactly the haproxy01p reference", len(d.MissingTargets))
	}
	m := d.MissingTargets[0]
	if m.SourceHost != "hyper01p" || m.SourceVM != "haproxy01p.domain.local" {
		t.Errorf("missing target names source %+v, want hyper01p:haproxy01p.domain.local", m)
	}
	if m.TargetHost != "hyper02p" || m.TargetVM != "haproxy01p.domain.local" {
		t.Errorf("missing target names target %q:%q, want the reference as the source's metadata wrote it", m.TargetHost, m.TargetVM)
	}
	if !m.PeerKnown {
		t.Error("PeerKnown is false although hyper02p has a reporting agent")
	}
	if m.PeerStale {
		t.Error("PeerStale is true although hyper02p reported just now")
	}
	if d.Counts["missing-target"] != 1 {
		t.Errorf("missing-target count = %d, want 1 -- otherwise the verdict line stays green over a VM with no copy", d.Counts["missing-target"])
	}

	s := testServer(t)
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "Target missing") || !strings.Contains(html, "haproxy01p.domain.local") {
		t.Error("the source replicating nowhere is not called out on the page")
	}
	if !strings.Contains(html, "missing target") {
		t.Error("the verdict line does not count the source with no target")
	}
}

func TestBuildDashboardMissingTargetShowsAnUnknownPeer(t *testing.T) {
	// The target host has no agent at all. The reference is still shown
	// verbatim rather than resolved away: something has to name the
	// misconfiguration, and the host-level blind-spot line cannot point at
	// which source is affected.
	agents := []store.Agent{{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
		{Name: "haproxy01p.domain.local", ReplicaTargets: []string{"hyper02p:haproxy01p.domain.local"}, Status: "ok"},
	}}}

	d := BuildDashboard(agents, reports, now)
	if len(d.MissingAgents) != 1 || d.MissingAgents[0] != "hyper02p" {
		t.Fatalf("MissingAgents = %v, want [hyper02p]", d.MissingAgents)
	}
	if len(d.MissingTargets) != 1 {
		t.Fatalf("MissingTargets = %+v, want the one dangling reference shown as-is", d.MissingTargets)
	}
	m := d.MissingTargets[0]
	if m.PeerKnown {
		t.Error("PeerKnown is true although no agent reports for hyper02p")
	}
	if m.PeerStale {
		t.Error("PeerStale is true although there is no report to be stale")
	}
	if c := d.Counts["missing-target"]; c != 1 {
		t.Errorf("missing-target count = %d, want 1", c)
	}

	s := testServer(t)
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin},
		Active:    "dashboard",
		Dashboard: d,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); !strings.Contains(html, "no agent reports under that exact name") {
		t.Error("the row does not say the target name resolves to no agent")
	}
}

func TestBuildDashboardMissingTargetNotesAStalePeer(t *testing.T) {
	// hyper02p's report is old: the replica's absence is unconfirmed, but
	// hiding the row would trade a qualified warning for silence.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Add(-time.Hour).Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "haproxy01p.domain.local", ReplicaTargets: []string{"hyper02p:haproxy01p.domain.local"}, Status: "ok"},
		}},
		"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Add(-time.Hour).Unix(), Domains: []store.ReportDomain{
			{Name: "other", Status: "unreplicated"},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.MissingTargets) != 1 {
		t.Fatalf("got %d missing targets, want 1 with a stale-peer qualifier", len(d.MissingTargets))
	}
	if !d.MissingTargets[0].PeerKnown {
		t.Error("PeerKnown is false although hyper02p has an agent")
	}
	if !d.MissingTargets[0].PeerStale {
		t.Error("PeerStale is false although hyper02p was last seen an hour ago")
	}
}

func TestBuildDashboardHostMatchingIsExact(t *testing.T) {
	// Short refs against FQDN reports do NOT resolve: normalising them
	// would hide a misconfiguration instead of showing it. This mirrors a
	// real estate where the two metadata directions spell the peer
	// differently -- short on the source side, FQDN on the target side.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p.domain.local", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "hap01l.test.local", ReplicaTargets: []string{"hyper02p:hap01l.test.local"}, Status: "ok", Active: true},
			{Name: "haproxy01p.domain.local", ReplicaTargets: []string{"hyper02p:haproxy01p.domain.local"}, Status: "ok", Active: true},
		}},
		"tgt": {Hostname: "hyper02p.domain.local", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "hap01l.test.local", ReplicaSource: "hyper01p.domain.local:hap01l.test.local", Status: "ok", AgeSeconds: 60},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.Pairs) != 1 {
		t.Fatalf("got %d pairs, want the one target hyper02p reported", len(d.Pairs))
	}
	if !d.Pairs[0].SourceSeen {
		t.Error("SourceSeen is false although hyper01p reports the source under its exact FQDN")
	}
	// Only the short target name reads as agent-less: the FQDN source
	// reference resolves exactly.
	if len(d.MissingAgents) != 1 || d.MissingAgents[0] != "hyper02p" {
		t.Errorf("MissingAgents = %v, want just [hyper02p]", d.MissingAgents)
	}
	// hap01l's dangling short ref is covered by its pair -- the copy
	// demonstrably exists -- so only the genuinely replica-less
	// haproxy01p is reported.
	if len(d.MissingTargets) != 1 {
		t.Fatalf("MissingTargets = %+v, want only the haproxy01p reference", d.MissingTargets)
	}
	m := d.MissingTargets[0]
	if m.SourceVM != "haproxy01p.domain.local" {
		t.Errorf("missing target names source %q, want haproxy01p.domain.local", m.SourceVM)
	}
	if m.PeerKnown {
		t.Errorf("%+v claims a known peer although no agent reports under %q", m, m.TargetHost)
	}
	if got := m.TargetHost; got != "hyper02p" {
		t.Errorf("target host displays as %q, want the reference as written, not the report's FQDN", got)
	}
	if d.Counts["missing-target"] != 1 {
		t.Errorf("missing-target count = %d, want 1", d.Counts["missing-target"])
	}
}

func TestBuildDashboardMissingTargetDefersToAnExistingPair(t *testing.T) {
	// web01 replicates to two targets but only hyper02p's replica reports.
	// The hyper03p reference dangles, yet no row may claim "no copy": the
	// healthy pair proves a copy exists under that VM name. A lost second
	// copy while the first survives is a different signal than this panel;
	// conflating the two is what made 11+1+2 add up to 14.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01", "hyper03p:web01"}, Status: "ok", Active: true},
		}},
		"tgt": {Hostname: "hyper02p", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "web01", ReplicaSource: "hyper01p:web01", Status: "ok", AgeSeconds: 60},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(d.Pairs))
	}
	// The unheard-from host is still reported once, at host level.
	if len(d.MissingAgents) != 1 || d.MissingAgents[0] != "hyper03p" {
		t.Fatalf("MissingAgents = %v, want [hyper03p]", d.MissingAgents)
	}
	if len(d.MissingTargets) != 0 {
		t.Errorf("MissingTargets = %+v, want none -- the pair already proves web01 has a copy", d.MissingTargets)
	}
	if c := d.Counts["missing-target"]; c != 0 {
		t.Errorf("missing-target count = %d, want 0", c)
	}
}

func TestBuildDashboardSortsWorstFirst(t *testing.T) {
	// An availability page is read to find what needs attention. Burying a
	// critical pair below a page of healthy ones defeats the purpose.
	agents := []store.Agent{{ID: "a1", Hostname: "h", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {Hostname: "h", Domains: []store.ReportDomain{
		{Name: "ok1", ReplicaSource: "s:ok1", Status: "ok"},
		{Name: "crit1", ReplicaSource: "s:crit1", Status: "critical"},
		{Name: "warn1", ReplicaSource: "s:warn1", Status: "warning"},
		{Name: "paused1", ReplicaSource: "s:paused1", Status: "paused"},
	}}}

	d := BuildDashboard(agents, reports, now)
	if d.Pairs[0].Status != "critical" {
		t.Errorf("first row is %q, want critical", d.Pairs[0].Status)
	}
	if d.Pairs[len(d.Pairs)-1].Status != "ok" {
		t.Errorf("last row is %q, want ok", d.Pairs[len(d.Pairs)-1].Status)
	}
}

// pairAged builds a source/target pair whose SOURCE report is srcReportAge
// seconds old. The source's report age is load-bearing: its Active flag is
// a stored value that is never aged out, so a host that died while its VM
// was running keeps claiming Active forever.
func pairAged(role string, srcActive, tgtActive bool, srcReportAge int64) Dashboard {
	agents := []store.Agent{
		{ID: "src", Hostname: "prod", LastSeenAt: now.Unix() - srcReportAge},
		{ID: "tgt", Hostname: "dr", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "prod", ReportedAtUnix: now.Unix() - srcReportAge,
			Domains: []store.ReportDomain{
				{Name: "web01", ReplicaTargets: []string{"dr:web01"}, Active: srcActive, Status: "ok"},
			}},
		"tgt": {Hostname: "dr", ReportedAtUnix: now.Unix(),
			Domains: []store.ReportDomain{
				{Name: "web01", ReplicaSource: "prod:web01", Role: role, Active: tgtActive,
					Status: "ok", AgeSeconds: 300},
			}},
	}
	return BuildDashboard(agents, reports, now)
}

// TestSplitBrainRequiresACurrentSourceReport is the difference between an
// alarm worth reading and one that fires on every correct failover.
//
// The case this detector exists for is a forced promotion during a site
// outage -- which is also the case where the dead source's last stored
// report says Active:true and will say so forever, because the host that
// would correct it is gone. Asserting on the stored boolean alone would
// make the loudest thing on the page mean "a failover happened".
func TestSplitBrainRequiresACurrentSourceReport(t *testing.T) {
	fresh := pairAged("promoted", true, true, 30)
	if len(fresh.SplitBrain) != 1 {
		t.Fatal("both ends running and the source reporting 30s ago is a confirmed split-brain")
	}

	// The source died an hour ago with its VM running. This is what a
	// correctly-executed forced failover of a dead primary looks like.
	stale := pairAged("promoted", true, true, 3600)
	if len(stale.SplitBrain) != 0 {
		t.Error("raised a confirmed split-brain from an hour-old report -- this fires on every correct failover")
	}
	if len(stale.SplitBrainPossible) != 1 {
		t.Error("dropped the case entirely; silence about the source is not proof the source is down")
	}
	if got := stale.Pairs[0].SourceLastSeen(); got != "1h0m" {
		t.Errorf("SourceLastSeen = %q, want 1h0m so the operator can judge it", got)
	}
	if stale.Counts["split-brain"] != 0 {
		t.Errorf("counted an unconfirmed case as split-brain: %d", stale.Counts["split-brain"])
	}
}

// TestSplitBrainNeedsBothEndsRunning pins the exact condition. Too loose and
// the loudest thing on the page cries wolf during every ordinary failover;
// too tight and the one case it exists for goes unreported.
func TestSplitBrainNeedsBothEndsRunning(t *testing.T) {
	// pair builds a source/target pair with the given runtime states and the
	// given role on the target.
	pair := func(role string, srcActive, tgtActive bool) Dashboard {
		return pairAged(role, srcActive, tgtActive, 30)
	}

	for _, tc := range []struct {
		name                 string
		role                 string
		srcActive, tgtActive bool
		want                 bool
	}{
		{"promoted and both up", "promoted", true, true, true},
		{"promoted, old source down", "promoted", false, true, false},
		{"promoted but not started yet", "promoted", true, false, false},
		{"healthy pair, source up target down", "target", true, false, false},
		// A running target that has NOT been promoted is a different
		// problem -- the pairs table already flags it -- and calling it
		// split-brain here would fire on every ordinary misconfiguration.
		{"target running but never promoted", "target", true, true, false},
		{"paused, both up", "paused", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := pair(tc.role, tc.srcActive, tc.tgtActive)
			if got := len(d.SplitBrain) > 0; got != tc.want {
				t.Errorf("split-brain = %v, want %v", got, tc.want)
			}
			if len(d.Pairs) != 1 {
				t.Fatalf("got %d pairs, want 1", len(d.Pairs))
			}
			if got := d.Pairs[0].SplitBrain(); got != tc.want {
				t.Errorf("Pair.SplitBrain() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSplitBrainNeedsBothAgentsReporting: the condition is only knowable
// when both hosts are heard from. A promoted target whose old source has no
// agent must not be called split-brain -- nobody knows whether it is running.
func TestSplitBrainNeedsBothAgentsReporting(t *testing.T) {
	agents := []store.Agent{{ID: "tgt", Hostname: "dr", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"tgt": {Hostname: "dr", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
		{Name: "web01", ReplicaSource: "prod:web01", Role: "promoted", Active: true, Status: "ok"},
	}}}
	d := BuildDashboard(agents, reports, now)
	if len(d.SplitBrain) != 0 {
		t.Error("called it split-brain with no agent on the source host -- that is an unknown, not a fact")
	}
	if !d.Pairs[0].TargetActive || d.Pairs[0].SourceSeen {
		t.Errorf("pair = %+v, want the target running and the source unobserved", d.Pairs[0])
	}
	// It is still a blind spot, and must be reported as one.
	if len(d.MissingAgents) != 1 || d.MissingAgents[0] != "prod" {
		t.Errorf("MissingAgents = %v, want [prod]", d.MissingAgents)
	}
}

func TestSplitBrainOutranksEveryStatus(t *testing.T) {
	// A split-brain pair sorts above a critical one: "behind on replication"
	// and "two machines writing to one identity" are not the same kind of
	// problem, and the second must be read first.
	agents := []store.Agent{
		{ID: "src", Hostname: "prod", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "dr", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "prod", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "web01", ReplicaTargets: []string{"dr:web01"}, Active: true},
			{Name: "db01", ReplicaTargets: []string{"dr:db01"}, Active: true},
		}},
		"tgt": {Hostname: "dr", Domains: []store.ReportDomain{
			{Name: "db01", ReplicaSource: "prod:db01", Status: "critical", AgeSeconds: -1},
			{Name: "web01", ReplicaSource: "prod:web01", Role: "promoted", Active: true, Status: "promoted"},
		}},
	}
	d := BuildDashboard(agents, reports, now)
	if d.Pairs[0].TargetVM != "web01" {
		t.Errorf("first pair is %q, want the split-brain one ahead of the critical one", d.Pairs[0].TargetVM)
	}
	if d.Counts["split-brain"] != 1 {
		t.Errorf("split-brain count = %d, want 1", d.Counts["split-brain"])
	}
}

func TestStaleAgentIsMarked(t *testing.T) {
	// Stale data rendered as current is worse than no data: it invites a
	// decision made on a picture that is no longer true.
	agents := []store.Agent{
		{ID: "fresh", Hostname: "a", LastSeenAt: now.Unix() - 30},
		{ID: "stale", Hostname: "b", LastSeenAt: now.Add(-time.Hour).Unix()},
		{ID: "never", Hostname: "c"},
	}
	d := BuildDashboard(agents, map[string]store.Report{}, now)
	byHost := map[string]AgentView{}
	for _, a := range d.Agents {
		byHost[a.Hostname] = a
	}
	if byHost["a"].Stale {
		t.Error("a agent seen 30s ago was marked stale")
	}
	if !byHost["b"].Stale {
		t.Error("an agent last seen an hour ago was not marked stale")
	}
	if !byHost["c"].Stale || byHost["c"].LastSeen != "never" {
		t.Errorf("an agent that has never reported = %+v, want stale and \"never\"", byHost["c"])
	}
}
