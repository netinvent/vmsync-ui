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

// enrolled puts a real agent in the store and returns its minted id, because
// the id is minted at enrolment and cannot be chosen.
func enrolled(t *testing.T, s *Server, host string) string {
	t.Helper()
	tok, err := s.Store.CreateEnrolmentToken(host, "op", time.Hour)
	if err != nil {
		t.Fatalf("token for %s: %v", host, err)
	}
	a, _, err := s.Store.Enrol(tok, host, "0.40")
	if err != nil {
		t.Fatalf("enrol %s: %v", host, err)
	}
	return a.ID
}

func scheduleForm(agentID string) url.Values {
	return url.Values{
		"agent_id":         {agentID},
		"vm":               {"web01"},
		"interval_minutes": {"30"},
		"preset":           {"wan"},
		"verify":           {"full"},
		"target_disk_path": {"/data/replicas"},
		"target_host":      {"hyper02p"},
		"enabled":          {"on"},
	}
}

// Saving a schedule for a revoked agent reported success and then hid the row.
//
// BuildScheduleView skips revoked agents, and store.Authenticate answers one as
// not-found, so the entry could never be read or run. The operator was told
// "the agent picks this up on its next poll", reloaded, and found nothing --
// believing a VM was scheduled that was scheduled nowhere. The reachable case
// is a schedule tab left open across a revocation.
func TestSavingAScheduleForARevokedAgentIsRefused(t *testing.T) {
	s := testServer(t)
	agentID := enrolled(t, s, "hyper02p")

	// The control first, through the same endpoint and the same form: this is
	// what proves the refusal below is about the revocation and not about the
	// fixture.
	if rec := post(t, s, "/schedule/save", scheduleForm(agentID)); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving for a live agent returned %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	entries, err := s.Store.Schedules()
	if err != nil {
		t.Fatalf("schedules: %v", err)
	}
	if len(entries[agentID]) != 1 {
		t.Fatalf("a live agent's save stored %d entries, want 1", len(entries[agentID]))
	}
	if err := s.Store.DeleteScheduleEntry(agentID, "web01"); err != nil {
		t.Fatalf("clear the entry: %v", err)
	}

	if err := s.Store.Revoke(agentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	rec := post(t, s, "/schedule/save", scheduleForm(agentID))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("redirect was %q: a save for a revoked agent has to come back as an error, not a flash saying the agent will pick it up", loc)
	}
	if !strings.Contains(loc, "revoked") {
		t.Errorf("redirect was %q: the refusal does not say the agent was revoked, which is the one fact the operator needs to act on", loc)
	}
	if !strings.Contains(loc, "hyper02p") {
		t.Errorf("redirect was %q: the refusal does not name the host", loc)
	}

	entries, err = s.Store.Schedules()
	if err != nil {
		t.Fatalf("schedules: %v", err)
	}
	if n := len(entries[agentID]); n != 0 {
		t.Errorf("%d schedule entries were stored for a revoked agent, want 0 -- re-enrolling that host mints a new id, so nothing ever inherits or cleans up the orphan", n)
	}
}

// A run from a revoked agent stays on the board and loses its unqualified pill.
//
// The panel is read as "did the last few syncs work". A revoked agent's report
// is frozen, so its newest run stays newest and keeps its green tick for as
// long as the record survives -- indefinitely on a quiet estate. Dropping the
// row would be the other error: the run happened, on that host, and a history
// that silently omits entries is worse than one naming a host you remember
// withdrawing.
func TestARevokedAgentsRunsAreMarkedNotDropped(t *testing.T) {
	report := store.Report{
		Hostname: "hyper01p", ReportedAtUnix: now.Unix() - 30,
		Syncs: []store.SyncResult{
			{VM: "web01", TargetHost: "hyper02p", StartedAtUnix: now.Unix() - 300, DurationSecs: 42, Outcome: store.OutcomeSuccess},
		},
	}

	t.Run("a live agent's run is not marked", func(t *testing.T) {
		runs := BuildRunView(
			[]store.Agent{{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix()}},
			map[string]store.Report{"src": report}, now, 15)
		if len(runs) != 1 {
			t.Fatalf("got %d runs, want 1", len(runs))
		}
		if runs[0].Withdrawn {
			t.Error("a live agent's run is marked withdrawn")
		}
	})

	t.Run("a revoked agent's run is kept and marked", func(t *testing.T) {
		runs := BuildRunView(
			[]store.Agent{{ID: "src", Hostname: "hyper01p", LastSeenAt: now.Unix(), Revoked: true, RevokedAt: now.Unix()}},
			map[string]store.Report{"src": report}, now, 15)
		if len(runs) != 1 {
			t.Fatalf("got %d runs, want 1 -- the run happened, and dropping it makes the history lie by omission", len(runs))
		}
		if !runs[0].Withdrawn {
			t.Error("a revoked agent's run carries an unqualified pill, so a frozen report reads as current fleet health for as long as the record survives")
		}
		if !runs[0].OK {
			t.Error("the outcome was changed as well as marked: the run did succeed, and only its currency is in question")
		}
	})
}
