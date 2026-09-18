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
				{Name: "db01", ReplicaSource: "hyper01p:db01", Status: "critical", AgeSeconds: -1,
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
			{Name: "haproxy01p.badmin.local", ReplicaTargets: []string{"hyper02p:haproxy01p.badmin.local"}, Status: "ok", Active: true},
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
	if m.SourceHost != "hyper01p" || m.SourceVM != "haproxy01p.badmin.local" {
		t.Errorf("missing target names source %+v, want hyper01p:haproxy01p.badmin.local", m)
	}
	if m.TargetHost != "hyper02p" || m.TargetVM != "haproxy01p.badmin.local" {
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
	if !strings.Contains(html, "Target missing") || !strings.Contains(html, "haproxy01p.badmin.local") {
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
		{Name: "haproxy01p.badmin.local", ReplicaTargets: []string{"hyper02p:haproxy01p.badmin.local"}, Status: "ok"},
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
			{Name: "haproxy01p.badmin.local", ReplicaTargets: []string{"hyper02p:haproxy01p.badmin.local"}, Status: "ok"},
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
	// would hide a misconfiguration instead of showing it. Every join on
	// this page treats "hyper02p" and "hyper02p.badmin.local" as different
	// hosts, and every dangling reference shows up verbatim.
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p.badmin.local", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "hap01l.test.local", ReplicaTargets: []string{"hyper02p:hap01l.test.local"}, Status: "ok", Active: true},
			{Name: "haproxy01p.badmin.local", ReplicaTargets: []string{"hyper02p:haproxy01p.badmin.local"}, Status: "ok", Active: true},
		}},
		"tgt": {Hostname: "hyper02p.badmin.local", ReportedAtUnix: now.Unix(), Domains: []store.ReportDomain{
			{Name: "hap01l.test.local", ReplicaSource: "hyper01p:hap01l.test.local", Status: "ok", AgeSeconds: 60},
		}},
	}

	d := BuildDashboard(agents, reports, now)
	if len(d.Pairs) != 1 {
		t.Fatalf("got %d pairs, want the one target hyper02p reported", len(d.Pairs))
	}
	if d.Pairs[0].SourceSeen {
		t.Error("SourceSeen is true although nothing reports under the short name the replica_source uses")
	}
	if len(d.MissingAgents) != 2 || d.MissingAgents[0] != "hyper01p" || d.MissingAgents[1] != "hyper02p" {
		t.Errorf("MissingAgents = %v, want both short names -- neither resolves to an FQDN report", d.MissingAgents)
	}
	// Both sources dangle: haproxy01p's replica is genuinely gone, and
	// hap01l's resolves to nothing either under exact matching. Each row
	// names its misconfiguration instead of guessing.
	if len(d.MissingTargets) != 2 {
		t.Fatalf("MissingTargets = %+v, want both dangling references shown as-is", d.MissingTargets)
	}
	for _, m := range d.MissingTargets {
		if m.PeerKnown {
			t.Errorf("%+v claims a known peer although no agent reports under %q", m, m.TargetHost)
		}
		if got := m.TargetHost; got != "hyper02p" {
			t.Errorf("target host displays as %q, want the reference as written, not the report's FQDN", got)
		}
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
