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

import "testing"

// The three-step fallback, which this package and the agent both implement
// and must agree on. A VM that stops fine when an operator asks but fails
// when a fence does would be a genuinely baffling thing to debug.
func TestResolveShutdownTimeout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		perVM, estate int
		want          int
	}{
		{"the VM's own value wins", 900, 300, 900},
		{"the estate default when the VM has none", 0, 600, 600},
		{"vmsync's own default when neither is set", 0, 0, DefaultShutdownTimeoutSec},
		{"a per-VM value wins even when it is shorter", 60, 3600, 60},
		{"negatives are treated as unset, not as a value", -5, 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveShutdownTimeout(tc.perVM, tc.estate); got != tc.want {
				t.Errorf("ResolveShutdownTimeout(%d, %d) = %d, want %d", tc.perVM, tc.estate, got, tc.want)
			}
		})
	}
}

func TestValidateShutdownTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		sec  int
		ok   bool
	}{
		{"zero means inherit, which is the common case", 0, true},
		{"the minimum is allowed", MinShutdownTimeoutSec, true},
		{"the maximum is allowed", MaxShutdownTimeoutSec, true},
		{"an ordinary value", 300, true},
		{"too short for ACPI to even reach a guest's init system", 5, false},
		{"long enough to wedge a failover behind one domain", 7200, false},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateShutdownTimeout(tc.sec)
			if tc.ok && err != nil {
				t.Errorf("ValidateShutdownTimeout(%d) = %v, want accepted", tc.sec, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateShutdownTimeout(%d) was accepted, want refused", tc.sec)
			}
		})
	}
}

// A default of zero would silently hand every shutdown back to vmsync's own
// number, which is fine right up until somebody assumes the console's
// setting is the one in force.
func TestDefaultSettingsCarryAShutdownTimeout(t *testing.T) {
	if got := DefaultSettings().ShutdownTimeoutSec; got != DefaultShutdownTimeoutSec {
		t.Errorf("DefaultSettings().ShutdownTimeoutSec = %d, want %d", got, DefaultShutdownTimeoutSec)
	}
}

// The estate default has to REACH agents, because a fence resolves its own
// timeout with no operation to carry one: there is no control plane to ask
// during the partition that usually causes a fence.
func TestTheEstateShutdownTimeoutReachesTheAgentConfig(t *testing.T) {
	s, srcID, _ := twoAgents(t)

	set, err := s.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if set.ShutdownTimeoutSec != DefaultShutdownTimeoutSec {
		t.Fatalf("a fresh store has ShutdownTimeoutSec %d, want the default %d",
			set.ShutdownTimeoutSec, DefaultShutdownTimeoutSec)
	}
	set.ShutdownTimeoutSec = 900
	if err := s.SetSettings(set); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	cfg, _, err := s.AgentConfigFor(srcID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if cfg.ShutdownTimeoutSec != 900 {
		t.Errorf("the agent is told %ds, want 900 -- without this a fence has no estate default to fall back on",
			cfg.ShutdownTimeoutSec)
	}
}

// A per-VM override travels on the schedule entry, which is the only per-VM
// channel an agent already receives.
func TestAPerVMShutdownTimeoutReachesTheAgentConfig(t *testing.T) {
	s, srcID, _ := twoAgents(t)

	if err := s.SetScheduleEntry(srcID, ScheduleEntry{
		VM: "db01", IntervalSeconds: 900, Enabled: true,
		ShutdownTimeoutSec: 1200,
	}); err != nil {
		t.Fatalf("SetScheduleEntry: %v", err)
	}

	cfg, _, err := s.AgentConfigFor(srcID)
	if err != nil {
		t.Fatalf("AgentConfigFor: %v", err)
	}
	if len(cfg.Schedule) != 1 {
		t.Fatalf("got %d schedule entries, want 1", len(cfg.Schedule))
	}
	if cfg.Schedule[0].ShutdownTimeoutSec != 1200 {
		t.Errorf("the per-VM override arrived as %d, want 1200", cfg.Schedule[0].ShutdownTimeoutSec)
	}
}
