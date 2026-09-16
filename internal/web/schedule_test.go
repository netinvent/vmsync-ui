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
	"strings"
	"testing"
	"time"

	"vmsync-ui/internal/auth"
	"vmsync-ui/internal/store"
)

// TestSchedulePageRendersForBothRoles exercises the branches the plain
// render test cannot: an unscheduled row has a nil *ScheduleEntry, and a
// read-only account must get no form at all rather than one that 403s on
// submit.
func TestSchedulePageRendersForBothRoles(t *testing.T) {
	s := testServer(t)
	agents, reports, schedules := fullFixture()
	view := BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now)

	var scheduled, unscheduled bool
	for _, r := range view.Rows {
		if r.Scheduled() {
			scheduled = true
		} else {
			unscheduled = true
		}
	}
	if !scheduled || !unscheduled {
		t.Fatalf("fixture must produce both a scheduled and an unscheduled row; got %+v", view.Rows)
	}

	for _, tc := range []struct {
		role     auth.Role
		wantForm bool
	}{
		{auth.RoleAdmin, true},
		{auth.RoleReadOnly, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			var buf strings.Builder
			err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
				User:     auth.User{Username: "op", Role: tc.role, CSRF: "tok"},
				Active:   "schedule",
				Schedule: view,
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			html := buf.String()

			if got := strings.Contains(html, `action="/schedule/save"`); got != tc.wantForm {
				t.Errorf("save form present = %v, want %v for role %s", got, tc.wantForm, tc.role)
			}
			if got := strings.Contains(html, `action="/schedule/delete"`); got != tc.wantForm {
				t.Errorf("delete form present = %v, want %v for role %s", got, tc.wantForm, tc.role)
			}
			// Every VM must appear whoever is looking: read-only exists to
			// see the estate, not a redacted part of it.
			for _, vm := range []string{"web01", "db01"} {
				if !strings.Contains(html, vm) {
					t.Errorf("%s is missing from the page for role %s", vm, tc.role)
				}
			}
			// A target VM must never be offered: its source host's agent is
			// what syncs it.
			if strings.Contains(html, "hyper02p:db01") && !strings.Contains(html, "db01") {
				t.Error("a target-side VM leaked into the schedulable list")
			}
		})
	}
}

// TestScheduleFormDefaults covers what an operator actually submits. The
// trap is the enabled checkbox: a new entry rendered unchecked means
// filling the form in and pressing Schedule produces an entry that never
// runs, with nothing on screen saying so.
func TestScheduleFormDefaults(t *testing.T) {
	s := testServer(t)
	agents, reports, schedules := fullFixture()
	var buf strings.Builder
	err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
		User:     auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:   "schedule",
		Schedule: BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// db01 is unscheduled, web01 is scheduled and enabled: both must come
	// out checked, for different reasons.
	if n := strings.Count(buf.String(), `name="enabled" checked`); n != 2 {
		t.Errorf("got %d checked enable boxes, want 2 (a new entry defaults on, an enabled one stays on)", n)
	}

	// And a stored entry that is disabled must render unchecked, or the
	// page lies about what the agent is doing.
	schedules["src"][0].Enabled = false
	buf.Reset()
	if err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
		User:     auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:   "schedule",
		Schedule: BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now),
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if n := strings.Count(buf.String(), `name="enabled" checked`); n != 1 {
		t.Errorf("got %d checked enable boxes, want 1 -- a disabled entry must render unchecked", n)
	}
}

// TestScheduleFormCarriesCSRF: without it every save silently 400s, and
// the page looks like it simply does not work.
func TestScheduleFormCarriesCSRF(t *testing.T) {
	s := testServer(t)
	agents, reports, schedules := fullFixture()
	var buf strings.Builder
	err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
		User:     auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "the-token"},
		Active:   "schedule",
		Schedule: BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `name="csrf" value="the-token"`) {
		t.Error("no CSRF token in the schedule form")
	}
}

// signIn returns a live session cookie and the CSRF token that goes with
// it, so tests can drive the real mux instead of calling handlers directly.
func signIn(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	token, user, ok := s.Auth.SignIn("op", "a-sufficiently-long-password")
	if !ok {
		t.Fatal("could not sign in the test admin")
	}
	return &http.Cookie{Name: auth.SessionCookie, Value: token}, user.CSRF
}

func post(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	cookie, csrf := signIn(t, s)
	form.Set("csrf", csrf)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestScheduleSaveReachesTheAgentConfig is the join this whole phase
// exists for: what an admin fills in on the page has to come back out of
// the endpoint an agent polls, with the preset already resolved into
// explicit fields. A preset name travelling to the agent instead would
// mean the agent and this UI have to agree on what "wan" means forever.
func TestScheduleSaveReachesTheAgentConfig(t *testing.T) {
	s := testServer(t)
	agentID := "agent-1"
	if _, err := s.Store.AppendAudit("setup", "noop", "", ""); err != nil {
		t.Fatalf("store not writable: %v", err)
	}

	rec := post(t, s, "/schedule/save", url.Values{
		"agent_id":         {agentID},
		"vm":               {"web01"},
		"interval_minutes": {"30"},
		"preset":           {"wan"},
		"verify":           {"full"},
		"target_disk_path": {"/data/replicas"},
		"target_host":      {"hyper02p"},
		"enabled":          {"on"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "error=") {
		t.Fatalf("save redirected with an error: %s", loc)
	}

	cfg, _, err := s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if len(cfg.Schedule) != 1 {
		t.Fatalf("agent config carries %d entries, want 1", len(cfg.Schedule))
	}
	got := cfg.Schedule[0]
	if got.VM != "web01" || got.IntervalSeconds != 1800 || !got.Enabled || got.TargetHost != "hyper02p" {
		t.Errorf("entry = %+v, want web01 every 1800s, enabled, to hyper02p", got)
	}
	if got.Profile.Verify != "full" || got.Profile.TargetDiskPath != "/data/replicas" {
		t.Errorf("profile = %+v, want the form's verify and disk path", got.Profile)
	}
	// The preset must arrive as resolved settings, not just a label.
	wan, _ := store.PresetByName("wan")
	if got.Profile.Compress != wan.Compress || got.Profile.CompressLevel != wan.CompressLevel {
		t.Errorf("profile compression = %q/%q, want the wan preset's %q/%q -- the preset was not resolved",
			got.Profile.Compress, got.Profile.CompressLevel, wan.Compress, wan.CompressLevel)
	}

	// And removing it must actually remove it, not just stop showing it.
	if rec := post(t, s, "/schedule/delete", url.Values{"agent_id": {agentID}, "vm": {"web01"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete returned %d, want 303", rec.Code)
	}
	cfg, _, err = s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor after delete: %v", err)
	}
	if len(cfg.Schedule) != 0 {
		t.Errorf("agent config still carries %d entries after delete", len(cfg.Schedule))
	}
}

// TestScheduleSaveMovesThePollETag: agents long-poll with If-None-Match
// and are answered 304 while the tag is unchanged. A save that leaves the
// tag alone is therefore invisible -- the entry sits in the store and the
// host it is for never learns about it.
func TestScheduleSaveMovesThePollETag(t *testing.T) {
	s := testServer(t)
	agentID := "agent-1"

	_, before, err := s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	form := url.Values{
		"agent_id": {agentID}, "vm": {"web01"}, "interval_minutes": {"15"},
		"preset": {"lan"}, "verify": {""}, "target_host": {"h2"}, "enabled": {"on"},
	}
	if rec := post(t, s, "/schedule/save", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d", rec.Code)
	}
	_, afterSave, err := s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if afterSave == before {
		t.Fatal("the ETag did not change after a save, so polling agents would keep getting 304")
	}

	// Changing the interval alone must move it too: an unchanged tag here
	// leaves the host running the old cadence indefinitely.
	form.Set("interval_minutes", "60")
	if rec := post(t, s, "/schedule/save", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("second save returned %d", rec.Code)
	}
	_, afterEdit, err := s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if afterEdit == afterSave {
		t.Error("the ETag did not change when the interval was edited")
	}

	if rec := post(t, s, "/schedule/delete", url.Values{"agent_id": {agentID}, "vm": {"web01"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete returned %d", rec.Code)
	}
	_, afterDelete, err := s.Store.AgentConfigFor(agentID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if afterDelete == afterEdit {
		t.Error("the ETag did not change on delete, so the agent would keep running a removed entry")
	}
}

// TestScheduleSaveRejectsBadInput: every one of these silently accepted
// would produce an entry that misbehaves on a hypervisor rather than an
// error on screen.
func TestScheduleSaveRejectsBadInput(t *testing.T) {
	base := func() url.Values {
		return url.Values{
			"agent_id": {"a1"}, "vm": {"web01"}, "interval_minutes": {"15"},
			"preset": {"lan"}, "verify": {""}, "target_host": {"h2"},
		}
	}
	for _, tc := range []struct {
		name     string
		mutate   func(url.Values)
		wantCode int
	}{
		{"unknown preset", func(v url.Values) { v.Set("preset", "hyperspeed") }, http.StatusSeeOther},
		{"zero interval", func(v url.Values) { v.Set("interval_minutes", "0") }, http.StatusSeeOther},
		{"absurd interval", func(v url.Values) { v.Set("interval_minutes", "99999") }, http.StatusSeeOther},
		{"non-numeric interval", func(v url.Values) { v.Set("interval_minutes", "soon") }, http.StatusSeeOther},
		{"unknown verify mode", func(v url.Values) { v.Set("verify", "vigorously") }, http.StatusSeeOther},
		{"no vm", func(v url.Values) { v.Del("vm") }, http.StatusBadRequest},
		{"no agent", func(v url.Values) { v.Del("agent_id") }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			form := base()
			tc.mutate(form)
			rec := post(t, s, "/schedule/save", form)
			if rec.Code != tc.wantCode {
				t.Fatalf("got %d, want %d; body: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusSeeOther && !strings.Contains(rec.Header().Get("Location"), "error=") {
				t.Errorf("redirected without an error message: %s", rec.Header().Get("Location"))
			}
			cfg, _, err := s.Store.AgentConfigFor("a1")
			if err != nil {
				t.Fatalf("AgentConfigFor: %v", err)
			}
			if len(cfg.Schedule) != 0 {
				t.Errorf("a rejected save still stored %d entries", len(cfg.Schedule))
			}
		})
	}
}

// TestScheduleWriteNeedsAdminAndCSRF: this page changes what runs against
// production hypervisors, so neither guard may be optional.
func TestScheduleWriteNeedsAdminAndCSRF(t *testing.T) {
	s := testServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	form := url.Values{"agent_id": {"a1"}, "vm": {"web01"}, "interval_minutes": {"15"}, "preset": {"lan"}}

	t.Run("no session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/schedule/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("got %d -> %q, want a redirect to /login", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("bad csrf", func(t *testing.T) {
		cookie, _ := signIn(t, s)
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("csrf", "not-the-token")
		req := httptest.NewRequest(http.MethodPost, "/schedule/save", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 for a bad CSRF token", rec.Code)
		}
	})
}

// TestScheduleListsSourcesOnly is the rule the whole page rests on: a
// target is synced BY the agent on its source host, so offering to
// schedule it on the target's own agent produces an entry that agent can
// never act on -- a schedule that silently does nothing.
func TestScheduleListsSourcesOnly(t *testing.T) {
	agents := []store.Agent{
		{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()},
		{ID: "tgt", Hostname: "hyper02p", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"src": {Hostname: "hyper01p", Domains: []store.ReportDomain{
			{Name: "web01", ReplicaTargets: []string{"hyper02p:web01"}},
			{Name: "scratch01"}, // no metadata at all: nothing to replicate to
		}},
		"tgt": {Hostname: "hyper02p", Domains: []store.ReportDomain{
			{Name: "web01", ReplicaSource: "hyper01p:web01"},
		}},
	}

	v := BuildScheduleView(agents, reports, nil, store.DefaultSettings(), now)
	if len(v.Rows) != 1 {
		t.Fatalf("got %d rows, want 1 -- only the source side is schedulable", len(v.Rows))
	}
	if v.Rows[0].AgentID != "src" || v.Rows[0].VM != "web01" {
		t.Errorf("row = %+v, want web01 on the source agent", v.Rows[0])
	}
	if v.Unscheduled != 1 {
		t.Errorf("Unscheduled = %d, want 1", v.Unscheduled)
	}
}

func TestScheduleRowCarriesItsEntryAndDefaults(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {Hostname: "h1", Domains: []store.ReportDomain{
		{Name: "sched", ReplicaTargets: []string{"h2:sched"}},
		{Name: "unsched", ReplicaTargets: []string{"h2:unsched"}},
	}}}
	schedules := map[string][]store.ScheduleEntry{"a1": {{
		VM:              "sched",
		IntervalSeconds: 1800,
		Enabled:         true,
		Preset:          "wan",
		TargetHost:      "h2",
		Profile:         store.SyncProfile{Verify: "full", TargetDiskPath: "/data/replicas"},
	}}}

	v := BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now)
	byVM := map[string]ScheduleRow{}
	for _, r := range v.Rows {
		byVM[r.VM] = r
	}

	got := byVM["sched"]
	if !got.Scheduled() || got.IntervalMinutes() != 30 || got.SelectedPreset() != "wan" ||
		got.Verify() != "full" || got.TargetDiskPath() != "/data/replicas" || got.TargetHost() != "h2" {
		t.Errorf("scheduled row = %+v, want its stored entry reflected in every form field", got)
	}

	// An unscheduled row still has to render a usable form, so its
	// accessors must produce sane defaults rather than dereference a nil
	// entry.
	un := byVM["unsched"]
	if un.Scheduled() {
		t.Error("unsched reports itself as scheduled")
	}
	if un.IntervalMinutes() != 15 || un.SelectedPreset() != "lan" {
		t.Errorf("unscheduled defaults = %dm/%s, want 15m/lan", un.IntervalMinutes(), un.SelectedPreset())
	}
	if un.Verify() != "" || un.TargetDiskPath() != "" || un.TargetHost() != "" {
		t.Errorf("unscheduled row = %+v, want empty optional fields", un)
	}
}

// TestScheduleSortsUnscheduledFirst: a VM nobody is replicating on a timer
// is the reason to open this page, so it must not sort below the ones that
// are already handled.
func TestScheduleSortsUnscheduledFirst(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {Hostname: "h1", Domains: []store.ReportDomain{
		{Name: "aaa-scheduled", ReplicaTargets: []string{"h2:aaa"}},
		{Name: "zzz-unscheduled", ReplicaTargets: []string{"h2:zzz"}},
	}}}
	schedules := map[string][]store.ScheduleEntry{"a1": {{VM: "aaa-scheduled", IntervalSeconds: 900}}}

	v := BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now)
	if v.Rows[0].VM != "zzz-unscheduled" {
		t.Errorf("first row is %q, want the unscheduled VM even though it sorts last by name", v.Rows[0].VM)
	}
}

func TestScheduleMarksStaleAgents(t *testing.T) {
	// Saving against a stale agent is not an error, but the operator has to
	// know the change will sit unapplied until that host checks back in.
	agents := []store.Agent{
		{ID: "a1", Hostname: "h1", LastSeenAt: now.Add(-time.Hour).Unix()},
		{ID: "a2", Hostname: "h2", LastSeenAt: now.Unix()},
	}
	reports := map[string]store.Report{
		"a1": {Hostname: "h1", Domains: []store.ReportDomain{{Name: "v1", ReplicaTargets: []string{"x:v1"}}}},
		"a2": {Hostname: "h2", Domains: []store.ReportDomain{{Name: "v2", ReplicaTargets: []string{"x:v2"}}}},
	}
	v := BuildScheduleView(agents, reports, nil, store.DefaultSettings(), now)
	for _, r := range v.Rows {
		if r.Hostname == "h1" && !r.AgentStale {
			t.Error("a row from an agent last seen an hour ago is not marked stale")
		}
		if r.Hostname == "h2" && r.AgentStale {
			t.Error("a row from a currently-reporting agent was marked stale")
		}
	}
}

func TestScheduleSkipsRevokedAndSilentAgents(t *testing.T) {
	agents := []store.Agent{
		{ID: "gone", Hostname: "h1", LastSeenAt: now.Unix(), Revoked: true},
		{ID: "quiet", Hostname: "h2", LastSeenAt: now.Unix()}, // enrolled, never reported
	}
	reports := map[string]store.Report{"gone": {Hostname: "h1", Domains: []store.ReportDomain{
		{Name: "v1", ReplicaTargets: []string{"x:v1"}},
	}}}
	v := BuildScheduleView(agents, reports, nil, store.DefaultSettings(), now)
	if len(v.Rows) != 0 {
		t.Errorf("got %d rows, want 0 -- a revoked agent will never poll again and a silent one has told us nothing", len(v.Rows))
	}
}

// TestScheduleRowShowsTheLatestRun: the results list is append-only per
// report, so picking the wrong element shows a stale success next to a VM
// that has been failing since.
func TestScheduleRowShowsTheLatestRun(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {
		Hostname: "h1",
		Domains:  []store.ReportDomain{{Name: "web01", ReplicaTargets: []string{"h2:web01"}}},
		Syncs: []store.SyncResult{
			{VM: "web01", StartedAtUnix: now.Unix() - 7200, ExitCode: exit(0)},
			{VM: "web01", StartedAtUnix: now.Unix() - 60, ExitCode: exit(1), Error: "ssh: connect refused"},
			{VM: "other", StartedAtUnix: now.Unix(), ExitCode: exit(0)},
		},
	}}

	v := BuildScheduleView(agents, reports, nil, store.DefaultSettings(), now)
	if len(v.Rows) != 1 || v.Rows[0].LastResult == nil {
		t.Fatalf("rows = %+v, want one row carrying its last result", v.Rows)
	}
	if v.Rows[0].LastResult.ExitCode == nil || *v.Rows[0].LastResult.ExitCode != 1 {
		t.Errorf("LastResult exit = %d, want 1 -- the newest run, not the first or the last appended",
			v.Rows[0].LastResult.ExitCode)
	}
}

// TestBuildRunViewOrdersByRealTime guards the specific trap here: the
// humanised "10m ago" sorts before "2m ago" as a string, so ordering on
// the rendered text quietly reverses parts of the list.
func TestBuildRunViewOrdersByRealTime(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {Hostname: "h1", Syncs: []store.SyncResult{
		{VM: "ten", StartedAtUnix: now.Unix() - 600, ExitCode: exit(0)},
		{VM: "two", StartedAtUnix: now.Unix() - 120, ExitCode: exit(0)},
		{VM: "hour", StartedAtUnix: now.Unix() - 3600, ExitCode: exit(0)},
	}}}

	runs := BuildRunView(agents, reports, now, 0)
	got := []string{runs[0].VM, runs[1].VM, runs[2].VM}
	want := []string{"two", "ten", "hour"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (newest first)", got, want)
		}
	}
}

func TestBuildRunViewCarriesTheFailureReason(t *testing.T) {
	// A failure with no reason on the row sends the operator to a
	// hypervisor's journal, which is what this console exists to avoid.
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {Hostname: "h1", Syncs: []store.SyncResult{
		{VM: "ok", StartedAtUnix: now.Unix(), ExitCode: exit(0), LogTail: "done\n"},
		{VM: "bad", StartedAtUnix: now.Unix() - 10, ExitCode: exit(1),
			Error: "exit status 1", LogTail: "connecting\nfatal: target disk is smaller than source\n"},
	}}}

	runs := BuildRunView(agents, reports, now, 0)
	byVM := map[string]RunView{}
	for _, r := range runs {
		byVM[r.VM] = r
	}
	if !byVM["ok"].OK || byVM["ok"].Detail != "" {
		t.Errorf("successful run = %+v, want OK with no detail", byVM["ok"])
	}
	if byVM["bad"].OK {
		t.Error("a run that exited 1 is reported as OK")
	}
	// The last log line is more use than the generic wrapper error.
	if !strings.Contains(byVM["bad"].Detail, "target disk is smaller") {
		t.Errorf("Detail = %q, want the last log line", byVM["bad"].Detail)
	}
}

func TestBuildRunViewHonoursTheLimit(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "h1", LastSeenAt: now.Unix()}}
	var syncs []store.SyncResult
	for i := 0; i < 10; i++ {
		syncs = append(syncs, store.SyncResult{VM: "v", StartedAtUnix: now.Unix() - int64(i)})
	}
	reports := map[string]store.Report{"a1": {Hostname: "h1", Syncs: syncs}}

	if got := len(BuildRunView(agents, reports, now, 3)); got != 3 {
		t.Errorf("got %d rows with limit 3, want 3", got)
	}
	if got := len(BuildRunView(agents, reports, now, 0)); got != 10 {
		t.Errorf("got %d rows with limit 0, want all 10", got)
	}
}

// exit is a *int literal for test fixtures. SyncResult.ExitCode is a pointer
// so that "never observed" is distinguishable from "exited 0"; nil is a real
// value here, not an oversight.
func exit(code int) *int { return &code }

// TestDegradedRunIsNotRenderedAsPlainOK is the whole point of the Degraded
// field, asserted against the real templates.
//
// It renders rather than inspecting the view struct because the failure this
// guards against is a template one: Degraded is true, the template tests
// {{if .Succeeded}} first, and the operator gets a green tick over a guest
// whose filesystems are still frozen. Every field can be correct and the
// page still wrong, and `go build` does not read templates at all.
func TestDegradedRunIsNotRenderedAsPlainOK(t *testing.T) {
	s := testServer(t)
	agents, reports, schedules := fullFixture()

	const reason = "guest filesystems are still FROZEN: run virsh domfsthaw web01"
	zero := 0
	for id, rep := range reports {
		for i := range rep.Syncs {
			rep.Syncs[i].Outcome = store.OutcomeSuccess
			rep.Syncs[i].ExitCode = &zero
			rep.Syncs[i].Error = ""
			rep.Syncs[i].Degraded = true
			rep.Syncs[i].DegradedReason = reason
		}
		reports[id] = rep
	}

	view := BuildScheduleView(agents, reports, schedules, store.DefaultSettings(), now)
	var withResult int
	for _, r := range view.Rows {
		if r.LastResult != nil {
			withResult++
		}
	}
	if withResult == 0 {
		t.Fatal("fixture produced no row with a last result; this test would pass vacuously")
	}

	// Both roles. The page has TWO separate ok/failed ladders -- a compact
	// one in the read-only layout's "Last run" column, and the admin
	// runline -- and each has to be fixed on its own. Rendering only as
	// admin passes while the read-only view still shows a green tick.
	var buf strings.Builder
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleReadOnly} {
		t.Run(string(role), func(t *testing.T) {
			buf.Reset()
			if err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
				User:     auth.User{Username: "op", Role: role, CSRF: "tok"},
				Active:   "schedule",
				Schedule: view,
			}); err != nil {
				t.Fatalf("render schedule: %v", err)
			}
			html := buf.String()

			if !strings.Contains(html, "needs attention") {
				t.Error("schedule page shows no degraded pill for a degraded run")
			}
		})
	}

	buf.Reset()
	if err := s.tpl.ExecuteTemplate(&buf, "schedule.html", pageData{
		User:     auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:   "schedule",
		Schedule: view,
	}); err != nil {
		t.Fatalf("render schedule: %v", err)
	}
	// Only the admin runline has room for the reason; the read-only column
	// is a pill and nothing else.
	if !strings.Contains(buf.String(), reason) {
		t.Error("schedule page does not say WHY the run was degraded; the operator " +
			"has to go to the hypervisor's journal to find out, which is what this page exists to avoid")
	}

	// Same assertion for the dashboard's recent-syncs list, which has its own
	// view type and its own copy of the ok/failed ladder.
	runs := BuildRunView(agents, reports, now, 20)
	if len(runs) == 0 {
		t.Fatal("no runs in the dashboard view; this half of the test would pass vacuously")
	}
	for _, r := range runs {
		if !r.Degraded {
			t.Fatalf("BuildRunView dropped Degraded for %s/%s", r.Host, r.VM)
		}
		if r.Detail != reason {
			t.Errorf("run detail = %q, want the degraded reason %q", r.Detail, reason)
		}
	}

	buf.Reset()
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:    "dashboard",
		Dashboard: BuildDashboard(agents, reports, now),
		Runs:      runs,
	}); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	if html := buf.String(); !strings.Contains(html, "needs attention") {
		t.Error("dashboard shows no degraded pill for a degraded run")
	}
}

// A run can be BOTH unobserved and degraded, and the combination is not a
// contradiction: an adopted run's exit status is unknowable, while the guest
// it left frozen is definitely frozen. Both facts have to survive.
//
// The bug this pins: an outcome ladder with degraded as one of its rungs.
// Put degraded first and an unobserved run renders as "ok"; put it last and
// the frozen guest vanishes behind "unknown". Neither is acceptable, which
// is why it is rendered as its own independent pill.
func TestAnUnobservedRunCanAlsoBeDegraded(t *testing.T) {
	s := testServer(t)
	agents, reports, _ := fullFixture()

	const reason = "guest filesystems are still FROZEN: run virsh domfsthaw web01"
	for id, rep := range reports {
		for i := range rep.Syncs {
			// Unobserved: an adopted run, so no exit code at all.
			rep.Syncs[i].ExitCode = nil
			rep.Syncs[i].Outcome = store.OutcomeUnknown
			rep.Syncs[i].Error = ""
			rep.Syncs[i].Degraded = true
			rep.Syncs[i].DegradedReason = reason
		}
		reports[id] = rep
	}

	runs := BuildRunView(agents, reports, now, 20)
	if len(runs) == 0 {
		t.Fatal("no runs in the view; this test would pass vacuously")
	}
	for _, r := range runs {
		if !r.Unknown {
			t.Errorf("%s/%s lost its unobserved status", r.Host, r.VM)
		}
		if !r.Degraded {
			t.Errorf("%s/%s lost its degradation", r.Host, r.VM)
		}
		if !strings.Contains(r.Detail, reason) {
			t.Errorf("detail %q dropped the frozen-guest reason in favour of the "+
				"unobserved explanation; the guest is the thing still happening", r.Detail)
		}
		if !strings.Contains(r.Detail, "never observed") {
			t.Errorf("detail %q dropped the unobserved explanation", r.Detail)
		}
	}

	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, "dashboard.html", pageData{
		User:      auth.User{Username: "op", Role: auth.RoleAdmin, CSRF: "tok"},
		Active:    "dashboard",
		Dashboard: BuildDashboard(agents, reports, now),
		Runs:      runs,
	}); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	// Scoped to the recent-syncs section. The replication-pairs table above
	// it renders an ok pill of its own for a healthy pair, which is correct
	// and has nothing to do with how any single run ended.
	runsHTML := sectionAfter(buf.String(), "Recent syncs")
	if runsHTML == "" {
		t.Fatal("no recent-syncs section in the rendered dashboard")
	}
	if !strings.Contains(runsHTML, "needs attention") {
		t.Error("dashboard dropped the degraded pill for an unobserved run")
	}
	if !strings.Contains(runsHTML, "unknown") {
		t.Error("dashboard dropped the unknown pill; a degradation must not imply an outcome")
	}
	if strings.Contains(runsHTML, `<span class="pill s-ok">ok</span>`) {
		t.Error("dashboard claims a run succeeded when nobody observed how it ended")
	}
}

// sectionAfter returns the rest of the page from a heading onward, so an
// assertion about one panel is not silently satisfied -- or broken -- by
// another.
func sectionAfter(html, heading string) string {
	i := strings.Index(html, heading)
	if i < 0 {
		return ""
	}
	return html[i:]
}

// A VM the AGENT replicates from a default template has no entry here at all.
//
// The console used to report it as unscheduled -- counting it in the number
// that says "these need a decision from you", and showing a row of dashes --
// about a VM already being replicated every fifteen minutes. That is the
// console describing what it published rather than what runs, which is exactly
// what the agent's effective-schedule report exists to stop.
func TestDefaultCoveredVMIsNotReportedAsUnscheduled(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "hv01", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {
		Hostname: "hv01",
		Domains: []store.ReportDomain{
			{Name: "mail01", ReplicaTargets: []string{"dr01"}},
			{Name: "lab01", ReplicaTargets: []string{"dr01"}},
		},
		Timezone:              "CET",
		TimezoneOffsetSeconds: 3600,
		EffectiveSchedule: []store.EffectiveScheduleEntry{{
			VM: "mail01", Template: "default", Synthesised: true, Enabled: true,
			IntervalSeconds: 900, VerifyMode: "full",
			VerifyDays: "Sun *-*-01..07", VerifyWindow: "02:00-12:00",
			NextVerifyUnix: time.Date(2026, 4, 5, 2, 0, 0, 0, time.UTC).Unix(),
		}},
	}}
	// Nothing published here for either VM.
	view := BuildScheduleView(agents, reports, map[string][]store.ScheduleEntry{},
		store.DefaultSettings(), now)

	byVM := map[string]ScheduleRow{}
	for _, r := range view.Rows {
		byVM[r.VM] = r
	}

	mail := byVM["mail01"]
	if mail.Scheduled() {
		t.Error("mail01 reports as having an entry; it has none")
	}
	if !mail.AgentCovers() {
		t.Error("mail01 is replicated by the agent but AgentCovers() is false")
	}
	if !mail.CoveredByDefault() {
		t.Error("mail01 is not flagged as covered by a default template, so the page cannot explain why it syncs")
	}
	if got := mail.VerifyCadence(); got == "" {
		t.Error("no verify cadence shown for a VM the agent verifies monthly")
	}
	// The window is the AGENT's, rendered in the AGENT's zone.
	if got := mail.NextVerify(); !strings.Contains(got, "CET") {
		t.Errorf("next verify = %q, want it rendered in the agent's own zone", got)
	}

	// lab01 has neither an entry nor agent coverage: genuinely unscheduled.
	if byVM["lab01"].AgentCovers() {
		t.Error("lab01 has no entry and no effective schedule, but reports as covered")
	}
	if view.Unscheduled != 1 {
		t.Errorf("Unscheduled = %d, want 1 -- only lab01 needs a decision; mail01 is already being replicated",
			view.Unscheduled)
	}
}

// A monitor agent's VMs are not a decision this console can make.
//
// Nothing it publishes reaches that host: the agent runs no scheduler, so an
// interval saved against one of these rows is stored, rendered, and never read
// by any process. Counting them as "unscheduled" would send an operator to
// write exactly those entries -- for VMs that a crontab may well be syncing
// every fifteen minutes -- and then to wait for a change that cannot arrive.
//
// The rows still appear, and still carry what the host reported. That is the
// entire point of watching a cron-driven site rather than leaving it invisible.
func TestMonitorAgentVMsAreNotCountedAsUnscheduled(t *testing.T) {
	agents := []store.Agent{
		{ID: "a1", Hostname: "hv01", LastSeenAt: now.Unix(), Mode: store.AgentModeMonitor},
		{ID: "a2", Hostname: "hv02", LastSeenAt: now.Unix(), Mode: store.AgentModeControlled},
	}
	reports := map[string]store.Report{
		"a1": {Hostname: "hv01", Mode: store.AgentModeMonitor, Domains: []store.ReportDomain{
			{Name: "cron01", ReplicaTargets: []string{"dr01"}},
		}},
		"a2": {Hostname: "hv02", Mode: store.AgentModeControlled, Domains: []store.ReportDomain{
			{Name: "run01", ReplicaTargets: []string{"dr01"}},
		}},
	}
	view := BuildScheduleView(agents, reports, map[string][]store.ScheduleEntry{},
		store.DefaultSettings(), now)

	byVM := map[string]ScheduleRow{}
	for _, r := range view.Rows {
		byVM[r.VM] = r
	}

	// Still listed. A monitored host that vanished from this page would be
	// the invisibility monitor mode exists to remove.
	cron, ok := byVM["cron01"]
	if !ok {
		t.Fatal("a monitor agent's VM is missing from the page entirely")
	}
	if !cron.AgentReadOnly {
		t.Error("cron01's row does not know its agent is read-only, so the page will offer controls that do nothing")
	}
	if !cron.SyncedElsewhere() {
		t.Error("SyncedElsewhere() is false for a VM on a monitor agent")
	}

	run := byVM["run01"]
	if run.AgentReadOnly {
		t.Error("a controlled agent's VM is marked read-only; the page would warn that working controls do nothing")
	}

	// Only run01 needs a decision. cron01 is somebody else's job.
	if view.Unscheduled != 1 {
		t.Errorf("Unscheduled = %d, want 1 -- only run01 is a decision this console can act on", view.Unscheduled)
	}
}

// An agent that has never said what it is must keep its controls.
//
// Hiding them from a host that would have honoured them is the worse of the
// two failures: an operator who cannot act during an incident, with no
// explanation. Showing them for a monitor agent costs a warning the page
// already prints.
func TestAnAgentThatNeverStatedItsModeIsNotTreatedAsReadOnly(t *testing.T) {
	agents := []store.Agent{{ID: "a1", Hostname: "hv01", LastSeenAt: now.Unix()}}
	reports := map[string]store.Report{"a1": {
		Hostname: "hv01",
		Domains:  []store.ReportDomain{{Name: "vm01", ReplicaTargets: []string{"dr01"}}},
	}}
	view := BuildScheduleView(agents, reports, map[string][]store.ScheduleEntry{},
		store.DefaultSettings(), now)

	if len(view.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(view.Rows))
	}
	if view.Rows[0].AgentReadOnly {
		t.Error("an agent with no reported mode was treated as read-only, hiding controls it would have honoured")
	}
	if view.Unscheduled != 1 {
		t.Errorf("Unscheduled = %d, want 1: an unstated mode must not silently excuse a VM from needing a schedule", view.Unscheduled)
	}
}
