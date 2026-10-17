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
	"strconv"
	"testing"

	"vmsync-ui/internal/store"
)

// An agent reports "running" the moment it starts an operation. The page
// used to take any result as the end of the story, which made a failover in
// progress render as a finished one whose outcome was the word "running" --
// and, because only finished rows are capped, let a live operation be
// hidden by the history limit.
func TestARunningOperationRendersAsInFlight(t *testing.T) {
	agents, reports := failoverFixture()

	res := store.OperationResult{ID: "in-flight", Kind: store.OpPromote, VM: "db01", State: store.OpStateRunning}
	ops := []store.OperationRecord{{
		Operation: store.Operation{ID: "in-flight", Kind: store.OpPromote, VM: "db01",
			CreatedAtUnix: now.Unix() - 10, CreatedBy: "op", NotAfterUnix: now.Unix() + 600},
		AgentID: "dr", Result: &res,
	}}

	v := BuildFailoverView(agents, reports, ops, now)
	if len(v.Operations) != 1 {
		t.Fatalf("rendered %d operations, want 1", len(v.Operations))
	}
	row := v.Operations[0]
	if row.Terminal {
		t.Error("a running operation renders as terminal, so the console shows this failover as finished with the outcome \"running\" and never updates it")
	}
	if v.Pending != 1 {
		t.Errorf("Pending = %d, want 1 -- an operation underway is not counted as in flight anywhere on the page", v.Pending)
	}
	if row.Detail == "" {
		t.Error("the row has no detail, so an operator sees the bare word \"running\" with no indication it is still going")
	}
}

// ...and being non-terminal, it must be exempt from the history cap for the
// same reason a pending one is: a live operation hidden behind a limit is
// one blocking its VM with nothing on screen to show it.
func TestARunningOperationIsNeverCappedAway(t *testing.T) {
	agents, reports := failoverFixture()

	var ops []store.OperationRecord
	for i := 0; i < operationsShown*3; i++ {
		res := store.OperationResult{ID: "old-" + strconv.Itoa(i), State: store.OpStateDone}
		ops = append(ops, store.OperationRecord{
			Operation: store.Operation{ID: "old-" + strconv.Itoa(i), Kind: store.OpShutdown, VM: "web01",
				CreatedAtUnix: now.Unix() - int64(100+i), CreatedBy: "op"},
			AgentID: "src", Result: &res,
		})
	}
	// Oldest of all, and still going.
	running := store.OperationResult{ID: "ancient-but-running", State: store.OpStateRunning}
	ops = append(ops, store.OperationRecord{
		Operation: store.Operation{ID: "ancient-but-running", Kind: store.OpInvert, VM: "mail01",
			CreatedAtUnix: now.Unix() - 99999, CreatedBy: "op", NotAfterUnix: now.Unix() + 600},
		AgentID: "src", Result: &running,
	})

	v := BuildFailoverView(agents, reports, ops, now)

	shown := map[string]bool{}
	for _, o := range v.Operations {
		shown[o.ID] = true
	}
	if !shown["ancient-but-running"] {
		t.Error("an operation still running was hidden by the history cap; it blocks its VM and there is no row to see it on")
	}
	if v.Pending != 1 {
		t.Errorf("Pending = %d, want 1", v.Pending)
	}
}
