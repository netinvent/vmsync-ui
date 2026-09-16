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
	"encoding/json"
	"strings"
	"testing"
)

// The other half of the wire contract with the agent. These are the exact
// shapes vmsync-agent emits; the agent has a matching test asserting it emits
// them. Neither side can check the other, so both check the format.
func TestSyncResultDecodesTheAgentsWireFormat(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		wantSucceeded  bool
		wantUnobserved bool
	}{
		{
			// An adopted run: the agent never saw how it ended, so there is no
			// exit_code at all. This must NOT read as success.
			"an unobserved run",
			`{"vm":"web01","run_id":"r1","outcome":"unknown"}`,
			false, true,
		},
		{
			"a successful run",
			`{"vm":"web01","exit_code":0,"outcome":"success"}`,
			true, false,
		},
		{
			"a failed run",
			`{"vm":"web01","exit_code":1,"outcome":"failure","error":"exit status 1"}`,
			false, false,
		},
		{
			// vmsync stood down on lock contention. Neither a success nor a
			// failure, and rendering it as either is wrong.
			"a busy run",
			`{"vm":"web01","exit_code":75,"outcome":"busy"}`,
			false, false,
		},
		{
			// An agent older than the Outcome field: fall back to the exit
			// code, which those agents always populate.
			"a result from an older agent",
			`{"vm":"web01","exit_code":0}`,
			true, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r SyncResult
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := r.Succeeded(); got != tc.wantSucceeded {
				t.Errorf("Succeeded() = %v, want %v", got, tc.wantSucceeded)
			}
			if got := r.Unobserved(); got != tc.wantUnobserved {
				t.Errorf("Unobserved() = %v, want %v", got, tc.wantUnobserved)
			}
		})
	}
}

// The specific regression this pointer exists to prevent: an absent exit_code
// must not decode to 0 and read as a successful run.
func TestAnAbsentExitCodeIsNotSuccess(t *testing.T) {
	var r SyncResult
	if err := json.Unmarshal([]byte(`{"vm":"web01"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != nil {
		t.Fatalf("an absent exit_code decoded to %d, not nil", *r.ExitCode)
	}
	if r.Succeeded() {
		t.Error("a run with no observed exit code reported as succeeded; the console would show a green tick for a run nobody saw the end of")
	}
}

// A degraded run is a SUCCESS that still needs somebody. Succeeded() must
// keep saying true -- the sync did work, and anything keyed off it (staleness,
// failure counting, -reinit-after-failures) must not start treating a frozen
// guest as a replication failure. What changes is only that the console has
// something extra to say about it.
func TestDegradedIsCarriedWithoutChangingTheOutcome(t *testing.T) {
	const body = `{"vm":"db01","exit_code":0,"outcome":"success","degraded":true,` +
		`"degraded_reason":"the guest filesystems are still FROZEN: run virsh domfsthaw db01"}`

	var r SyncResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.Succeeded() {
		t.Error("a degraded run stopped counting as a success; that would make a failed " +
			"thaw look like a replication failure and drive reinit-after-failures")
	}
	if r.Unobserved() {
		t.Error("a degraded run read as unobserved")
	}
	if !r.Degraded {
		t.Error("degraded did not decode")
	}
	if r.DegradedReason == "" {
		t.Error("degraded_reason did not decode; the console would show a warning pill with nothing to act on")
	}
}

// An agent that predates the field must still decode, reporting no
// degradation rather than failing the whole report.
func TestAbsentDegradedIsNotADegradation(t *testing.T) {
	var r SyncResult
	if err := json.Unmarshal([]byte(`{"vm":"db01","exit_code":0,"outcome":"success"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Degraded || r.DegradedReason != "" {
		t.Errorf("a result with no degraded field reported one: %+v", r)
	}
}

// TestSuspendsSourceFailsSafe pins the DIRECTION of the check, which is the
// part that had a live bug.
//
// SuspendsSource gates the read-only role: a mode it calls harmless can be
// scheduled by an account that is not allowed to impact production. It used
// to be written as a list of the suspending modes -- `verify == "compare" ||
// verify == "fast"` -- and when the modes were renamed that list went stale
// in the worst possible direction: the new suspending mode, qemu-img, was
// not in it, so the function reported it as harmless and a read-only account
// could have paused a production guest.
//
// Written as an allowlist of the NON-suspending modes instead, so anything
// unrecognised is assumed to suspend. That is the answer that fails toward
// refusing rather than toward pausing somebody's database.
func TestSuspendsSourceFailsSafe(t *testing.T) {
	for _, tc := range []struct {
		verify string
		want   bool
	}{
		{"", false},
		{VerifyFast, false},
		{VerifyFull, false},
		{VerifyQemuImg, true},
		// The regression itself: a mode this build has never heard of --
		// a newer UI's value, a hand-edited store, a stale browser tab --
		// must be treated as suspending, not waved through.
		{"some-future-mode", true},
		{"online", true},
		{"compare", true},
	} {
		t.Run(tc.verify, func(t *testing.T) {
			if got := SuspendsSource(tc.verify); got != tc.want {
				t.Errorf("SuspendsSource(%q) = %v, want %v", tc.verify, got, tc.want)
			}
		})
	}
}

// Every mode the form can offer must survive validation, or the page shows
// an option that always errors on submit.
func TestEveryOfferedVerifyModeValidates(t *testing.T) {
	for _, v := range []string{"", VerifyFast, VerifyFull, VerifyQemuImg} {
		if err := ValidateProfile(SyncProfile{Verify: v}); err != nil {
			t.Errorf("ValidateProfile(verify=%q) = %v, want nil", v, err)
		}
	}
	// And a retired name must be refused here rather than travelling into a
	// schedule document, where an agent rejects the WHOLE document and stops
	// that host's entire schedule -- not just this one entry.
	if err := ValidateProfile(SyncProfile{Verify: "online"}); err == nil {
		t.Error("ValidateProfile accepted the retired mode \"online\"; it would reach an agent and take down that host's schedule")
	}
}

// no_checksum must round-trip through a profile, since that is the only way
// it can be set: it is deliberately not overridable from the schedule form
// (see handlers.go), so a preset or the agent's own JSON is where an
// operator turns the integrity check off.
//
// The direction that matters is the absent one. The field is negative --
// mirroring vmsync's -no-checksum and the agent's field -- specifically so
// that a profile which never mentions it leaves the check ON. A positive
// "checksum" would make every profile written before this field existed
// decode as disabling a safety feature.
func TestNoChecksumRoundTripsAndDefaultsToEnabled(t *testing.T) {
	var absent SyncProfile
	if err := json.Unmarshal([]byte(`{"compress":"zstd","compress_level":"3"}`), &absent); err != nil {
		t.Fatalf("unmarshal a profile without the field: %v", err)
	}
	if absent.NoChecksum {
		t.Error("a profile that does not mention no_checksum decoded as disabling the check")
	}

	// Omitted from the encoding when false, so an untouched profile does not
	// start carrying the field around.
	b, err := json.Marshal(absent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "no_checksum") {
		t.Errorf("no_checksum was encoded for a profile that leaves the check on: %s", b)
	}

	var disabled SyncProfile
	if err := json.Unmarshal([]byte(`{"no_checksum":true}`), &disabled); err != nil {
		t.Fatalf("unmarshal a profile that disables it: %v", err)
	}
	if !disabled.NoChecksum {
		t.Error("no_checksum:true did not decode as disabling the check")
	}
	if err := ValidateProfile(disabled); err != nil {
		t.Errorf("ValidateProfile rejected a profile that only disables the checksum: %v", err)
	}

	// And none of the shipped presets may disable it.
	for _, preset := range Presets {
		if preset.Profile.NoChecksum {
			t.Errorf("preset %q ships with the integrity check disabled", preset.Name)
		}
	}
}

// verify_failure_reinit must round-trip, must default to off, and must be
// refused without a verify mode.
//
// Unlike no_checksum this one IS offered on the schedule form, because it
// only ever adds a check. That makes the pairing rule load-bearing rather
// than theoretical: the form's two controls can be set independently, so
// "recopy once, then re-verify" with verification set to none is a
// combination an operator can actually produce with two clicks. It has to
// fail here, in the browser, and not travel into a schedule document -- an
// agent rejects the WHOLE document over one bad field, which stops that
// host's entire schedule rather than this one entry.
func TestVerifyFailureReinitRequiresAVerifyMode(t *testing.T) {
	var absent SyncProfile
	if err := json.Unmarshal([]byte(`{"verify":"full"}`), &absent); err != nil {
		t.Fatalf("unmarshal a profile without the field: %v", err)
	}
	if absent.VerifyFailureReinit {
		t.Error("a profile that never mentioned verify_failure_reinit decoded as enabling it")
	}

	var set SyncProfile
	if err := json.Unmarshal([]byte(`{"verify":"fast","verify_failure_reinit":true}`), &set); err != nil {
		t.Fatalf("unmarshal a profile with the field: %v", err)
	}
	if !set.VerifyFailureReinit {
		t.Error("verify_failure_reinit did not survive a JSON round trip")
	}

	// Every mode the form offers must accept it, or the page pairs two
	// controls that error against each other on submit.
	for _, v := range []string{VerifyFast, VerifyFull, VerifyQemuImg} {
		if err := ValidateProfile(SyncProfile{Verify: v, VerifyFailureReinit: true}); err != nil {
			t.Errorf("ValidateProfile(verify=%q, verify_failure_reinit) = %v, want nil", v, err)
		}
	}
	if err := ValidateProfile(SyncProfile{VerifyFailureReinit: true}); err == nil {
		t.Error("ValidateProfile accepted verify_failure_reinit with no verify mode; the agent would refuse the whole schedule document")
	}
}

// Every field is form-owned now, so SetScheduleEntry stores exactly what it
// is given and an omitted value clears.
//
// This used to assert the opposite, and the change is deliberate rather than a
// regression. The form rebuilds an entry from its own inputs and this replaces
// the stored one, so a field with no control was destroyed by an unrelated
// edit -- which is why Template, VerifyDays and VerifyWindow were preserved
// behind the caller's back. The schedule form now has a control for all
// three, so preserving would make "none" unexpressible: a VM could be given a
// verify window and never have it taken away.
func TestSetScheduleEntryStoresExactlyWhatItIsGiven(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const agent = "agent-1"

	// However it got there -- a hand-edited schedules.json, or a future
	// template editor -- this VM has a template and a monthly verify window.
	original := ScheduleEntry{
		VM: "web01", IntervalSeconds: 900, Enabled: true,
		Template:   "nightly",
		VerifyDays: "Sun *-*-01..07", VerifyWindow: "02:00-12:00",
		Profile: SyncProfile{Verify: "full"},
	}
	if err := s.SetScheduleEntry(agent, original); err != nil {
		t.Fatal(err)
	}

	// Now somebody changes the sync interval in the console. The form sends
	// everything it knows about and nothing it does not.
	edit := ScheduleEntry{
		VM: "web01", IntervalSeconds: 300, Enabled: true,
		Profile: SyncProfile{Verify: "full"},
	}
	if err := s.SetScheduleEntry(agent, edit); err != nil {
		t.Fatal(err)
	}

	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	var got ScheduleEntry
	for _, e := range sched[agent] {
		if e.VM == "web01" {
			got = e
		}
	}
	if got.IntervalSeconds != 300 {
		t.Errorf("IntervalSeconds = %d, want the edit's 300", got.IntervalSeconds)
	}
	// Template is NO LONGER preserved, and that is the current contract
	// rather than a regression: the schedule form has a template selector, so
	// an empty value means "no template" and carrying the stored one would
	// make that choice impossible to express. It preserved only while nothing
	// in the console could set it.
	if got.Template != "" {
		t.Errorf("Template = %q, want empty: an omitted value clears", got.Template)
	}
	if got.VerifyDays != "" || got.VerifyWindow != "" {
		t.Errorf("calendar = %q %q, want empty: the form owns these, so a submission without them clears them -- which is what makes \"no window\" expressible",
			got.VerifyDays, got.VerifyWindow)
	}
}

// And a NEW entry is stored as given, so preserving must not invent values.
func TestSetScheduleEntryStoresANewEntryAsGiven(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := ScheduleEntry{VM: "db01", IntervalSeconds: 600, Enabled: true}
	if err := s.SetScheduleEntry("agent-1", e); err != nil {
		t.Fatal(err)
	}
	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	got := sched["agent-1"][0]
	if got.Template != "" || got.VerifyDays != "" || got.VerifyWindow != "" {
		t.Errorf("a new entry came out with %+v; nothing should have been invented", got)
	}
}

// The template selector has to be able to say "none", and an empty value is
// how a <select> says it.
//
// carryUneditedFields preserved Template while nothing in the console could
// set it -- preserving beat destroying. With a selector on the schedule form
// that inverts: preserving would make "no template" unexpressible, so a VM
// could be linked to a template and never unlinked.
func TestScheduleEntryTemplateIsFormOwned(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const agent = "agent-1"

	linked := ScheduleEntry{VM: "web01", IntervalSeconds: 900, Enabled: true, Template: "nightly"}
	if err := s.SetScheduleEntry(agent, linked); err != nil {
		t.Fatal(err)
	}

	t.Run("an explicit template is stored", func(t *testing.T) {
		if got := entryFor(t, s, agent, "web01"); got.Template != "nightly" {
			t.Errorf("Template = %q, want %q", got.Template, "nightly")
		}
	})

	t.Run("selecting a different one replaces it", func(t *testing.T) {
		moved := linked
		moved.Template = "hourly"
		if err := s.SetScheduleEntry(agent, moved); err != nil {
			t.Fatal(err)
		}
		if got := entryFor(t, s, agent, "web01"); got.Template != "hourly" {
			t.Errorf("Template = %q, want %q", got.Template, "hourly")
		}
	})

	t.Run("selecting none clears it", func(t *testing.T) {
		unlinked := linked
		unlinked.Template = ""
		if err := s.SetScheduleEntry(agent, unlinked); err != nil {
			t.Fatal(err)
		}
		if got := entryFor(t, s, agent, "web01"); got.Template != "" {
			t.Errorf("Template = %q, want empty -- a VM linked to a template could never be unlinked", got.Template)
		}
	})
}

func entryFor(t *testing.T, s *Store, agent, vm string) ScheduleEntry {
	t.Helper()
	sched, err := s.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sched[agent] {
		if e.VM == vm {
			return e
		}
	}
	t.Fatalf("no entry for %s", vm)
	return ScheduleEntry{}
}
