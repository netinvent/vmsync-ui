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
	"strings"
	"testing"
)

// The grammar the engine writes, read back field by field.
//
// The keys are spelled out here on purpose. This is a wire contract between
// two separately-versioned programs that share no code, exactly like the
// report fields in the store, so the one thing a test of it must do is name
// the strings literally -- parsing a value this test built from its own
// constants would agree with itself no matter what the engine actually
// writes.
func TestThePartialCopyValueIsReadFieldByField(t *testing.T) {
	p := parsePartialCopy("verb=reinit,at=1758441600,action=9f3c1a2b4d5e6f70,host=hv-a,aside=1758441600")

	if !p.Readable {
		t.Fatal("a value in the engine's own documented grammar read as unreadable, so every affected row would show a warning it cannot explain")
	}
	if p.Verb != "reinit" {
		t.Errorf("Verb = %q, want reinit -- the verb is what says how much of the replica the dead run had already discarded", p.Verb)
	}
	if p.AtUnix != 1758441600 {
		t.Errorf("AtUnix = %d, want 1758441600", p.AtUnix)
	}
	if p.ActionID != "9f3c1a2b4d5e6f70" {
		t.Errorf("ActionID = %q, want the engine's correlation id -- it is the string to grep the hypervisor's journal for", p.ActionID)
	}
	if p.Host != "hv-a" {
		t.Errorf("Host = %q, want hv-a", p.Host)
	}
	if p.AsideSuffix() != ".vmsync-replaced-1758441600" {
		t.Errorf("AsideSuffix() = %q, want .vmsync-replaced-1758441600 -- this is the suffix an operator "+
			"types into an ls to find the complete copy the rebuild displaced", p.AsideSuffix())
	}
	if p.At() == "" {
		t.Error("the date did not render; a rebuild that died four minutes ago and one from last month need different responses")
	}
}

// Every verb the engine can arm the field with must read as understood. One
// this build does not know falls through to the unreadable path, which still
// warns but cannot say what the run was doing -- so a verb missing from the
// table is a real loss of explanation, not a cosmetic one.
func TestEveryVerbTheEngineArmsIsUnderstood(t *testing.T) {
	for _, verb := range []string{"reinit", "force-clean", "full-sync", "restore"} {
		p := parsePartialCopy("verb=" + verb + ",at=1758441600,action=abc,host=hv-a,aside=omitted")
		if !p.Readable {
			t.Errorf("verb %q read as unreadable", verb)
		}
		if p.Verb != verb {
			t.Errorf("Verb = %q, want %q", p.Verb, verb)
		}
	}
}

// An unreadable value must still be a warning. This is the direction the
// whole design rests on: the absence of this marker is what reads as "safe
// to promote", so anything that cannot be understood has to fail towards the
// alarm and never towards silence.
//
// Note what stays true in every one of these cases -- Raw survives, so the
// page can show the operator the exact text the agent reported and let them
// make of it what this build could not.
func TestAnUnreadablePartialCopyValueStillCountsAsOne(t *testing.T) {
	for _, raw := range []string{
		// A verb from a newer vmsync than this console.
		"verb=rebase-overlay,at=1758441600,action=abc,host=hv-a",
		// No verb at all.
		"at=1758441600,host=hv-a",
		// Not the grammar in any form: a truncated write, or a value from
		// something else entirely.
		"yes",
		"{\"verb\":\"reinit\"}",
		// Punctuation only, which is what a half-written metadata value can
		// decay to.
		",,,",
		"=",
	} {
		p := parsePartialCopy(raw)
		if p.Readable {
			t.Errorf("parsePartialCopy(%q) claimed to understand it", raw)
		}
		if p.Raw == "" {
			t.Errorf("parsePartialCopy(%q) dropped the raw text, so the page has nothing to show the operator", raw)
		}
	}
}

// Unknown keys are ignored rather than poisoning the parse, which the
// grammar requires of every reader. Without this, the first key a newer
// engine adds turns a warning this page exists to show into an unreadable
// blob on every affected row at once.
func TestUnknownKeysDoNotMakeAValueUnreadable(t *testing.T) {
	p := parsePartialCopy("verb=full-sync,at=1758441600,action=abc,host=hv-a,aside=1758441600,generation=3,mode=overlay")
	if !p.Readable {
		t.Fatal("a value carrying a key this build has never seen read as unreadable")
	}
	if p.Verb != "full-sync" || p.AtUnix != 1758441600 || p.Host != "hv-a" {
		t.Errorf("the known keys did not survive an unknown one: %+v", p)
	}
}

// aside=omitted means there is no displaced copy, and must not render as a
// filename. Telling an operator to look for files ending
// ".vmsync-replaced-omitted" would send them hunting for something that was
// never written, during an incident, which is worse than saying nothing.
func TestAnOmittedAsideStampNamesNoFiles(t *testing.T) {
	p := parsePartialCopy("verb=reinit,at=1758441600,action=abc,host=hv-a,aside=omitted")
	if !p.Readable {
		t.Fatal("the value should still be readable")
	}
	if p.AsideStamp != "" || p.AsideSuffix() != "" {
		t.Errorf("an omitted aside stamp produced %q / %q, want neither", p.AsideStamp, p.AsideSuffix())
	}
}

// A value carrying no readable time costs the date and nothing else. Same
// rule the verification record follows: a missing date renders as no date,
// never as 1970, and never as a reason to drop the warning.
func TestAnUnreadableTimeCostsOnlyTheDate(t *testing.T) {
	for _, raw := range []string{
		"verb=reinit,at=,action=abc,host=hv-a",
		"verb=reinit,at=soon,action=abc,host=hv-a",
		"verb=reinit,at=0,action=abc,host=hv-a",
		"verb=reinit,action=abc,host=hv-a",
	} {
		p := parsePartialCopy(raw)
		if !p.Readable {
			t.Errorf("parsePartialCopy(%q) dropped the whole value over its timestamp", raw)
		}
		if p.At() != "" {
			t.Errorf("parsePartialCopy(%q).At() = %q, want no date rather than a made-up one", raw, p.At())
		}
	}
}

// Nothing in, nothing out. An empty value is the state of almost every
// domain in an estate, and it must produce no marker at all -- a warning on
// every row is a warning nobody reads.
func TestAnEmptyValueIsNotAPartialCopy(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		p := parsePartialCopy(raw)
		if p.Readable || p.Raw != "" || p.Verb != "" || p.AtUnix != 0 {
			t.Errorf("parsePartialCopy(%q) invented a finding: %+v", raw, p)
		}
	}
}

// An oversized value is trimmed to the cap the engine itself applies when it
// writes one, so a garbled or hostile report cannot paste an unbounded blob
// into a page read during an incident. Trimmed, not discarded: what has to be
// acted on is that the marker EXISTS.
func TestAnOversizedValueIsTrimmedRatherThanDropped(t *testing.T) {
	p := parsePartialCopy("verb=reinit,at=1758441600,host=" + strings.Repeat("x", 4000))
	if p.Raw == "" {
		t.Fatal("an oversized value was dropped entirely, losing the warning with it")
	}
	if len(p.Raw) > maxIncompleteRawShown {
		t.Errorf("Raw kept %d bytes, want at most %d", len(p.Raw), maxIncompleteRawShown)
	}
}
