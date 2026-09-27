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
	"strings"
	"time"
)

// partialCopy is a domain's replica_incomplete value broken out FOR DISPLAY
// AND NOTHING ELSE.
//
// The distinction is the whole reason this type is safe to have here. The
// value is armed by the engine before it starts overwriting a replica's disks
// and cleared by the same metadata write that records the rebuild succeeding,
// so a domain still carrying it holds a half-written copy -- and the refusal
// that keeps such a copy from being promoted lives in vmsync, on the host
// holding those disks, because a promotion runs there during a disaster with
// the source host gone. Nothing on this page may become a second, weaker
// opinion about that: this console publishes instructions and enforces
// nothing, so what it does with the value is explain it.
//
// It is parsed here rather than shared with the engine because the two are
// separately-versioned programs that share no code, exactly like SyncProfile
// and Operation in the store. That makes the grammar a wire contract, which
// is why the keys are named literally in the tests.
type partialCopy struct {
	// Raw is the value exactly as the agent reported it, kept so an
	// unreadable one can still be shown. Evidence nobody can decode is worth
	// more than a blank cell.
	Raw string
	// Readable says this build understood the value. False means the marker
	// still renders -- a value from a newer engine, or a corrupted one, must
	// fail towards the warning and never towards silence.
	Readable bool
	// Verb is which action armed it: reinit, force-clean, full-sync or
	// restore. Worth showing because it says how much of the replica the
	// interrupted run had already thrown away.
	Verb string
	// AtUnix is when the run that died started. 0 when the value carried no
	// readable time, which renders as no date rather than as 1970.
	AtUnix int64
	// ActionID correlates with the journal the engine writes beside the
	// disks, and with this UI's own audit entry. It is the string to grep
	// the hypervisor for.
	ActionID string
	// Host is the machine that RAN the interrupted copy and was expected to
	// finish it -- the source for a reinit, a force-clean or a full sync,
	// and the replica's own host for a restore, which runs there. It is not
	// the machine the bytes were landing on: that is the row this marker is
	// attached to, and repeating it would say nothing. Shown because it
	// names who to ask, or says that the machine to ask is the one that is
	// gone.
	Host string
	// AsideStamp is the suffix the displaced disks were renamed with, so the
	// message can name the files the COMPLETE copy is still sitting in.
	// Empty when the run recorded none, which is the case where there is no
	// older copy to go back to.
	AsideStamp string
}

// knownIncompleteVerbs are the actions that arm the field, matching the
// engine's own grammar.
//
// Membership is what decides whether a value reads as understood. An
// unrecognised verb is treated as unreadable rather than shown as-is,
// because a verb this build has never heard of means the sentence built
// around it -- what the run was doing, and therefore what state it left the
// disks in -- would be guesswork presented as fact.
var knownIncompleteVerbs = map[string]bool{
	"reinit":      true,
	"force-clean": true,
	"full-sync":   true,
	"restore":     true,
}

// maxIncompleteRawShown bounds what an unreadable value may paste onto the
// page. It matches the cap the engine applies when it writes the value, so
// anything longer than this did not come from a vmsync that wrote this
// grammar at all, and is trimmed rather than rendered whole.
const maxIncompleteRawShown = 512

// parsePartialCopy reads the k=v value for display. Pure, stdlib only, and
// total: every input produces a value, because the caller's only alternative
// to rendering something is rendering nothing.
//
// Unknown keys are ignored by design -- the grammar says readers must
// tolerate them, so a newer engine adding one does not turn a warning this
// page exists to show into an unreadable blob.
func parsePartialCopy(raw string) partialCopy {
	p := partialCopy{Raw: strings.TrimSpace(raw)}
	if p.Raw == "" {
		return p
	}
	if len(p.Raw) > maxIncompleteRawShown {
		p.Raw = p.Raw[:maxIncompleteRawShown]
	}
	for _, field := range strings.Split(p.Raw, ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "verb":
			p.Verb = value
		case "at":
			// Errors are swallowed on purpose: a time nobody can read costs
			// the date and must not cost the warning.
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				p.AtUnix = n
			}
		case "action":
			p.ActionID = value
		case "host":
			p.Host = value
		case "aside":
			if value != "omitted" {
				p.AsideStamp = value
			}
		}
	}
	p.Readable = knownIncompleteVerbs[p.Verb]
	return p
}

// At renders when the interrupted rebuild started, or "" when the value
// carried no readable time.
//
// The date is half of what an operator needs. A rebuild that died four
// minutes ago is probably still recoverable by rerunning it; one from last
// month means somebody has been reading a green row for weeks, and the
// complete copy beside it may since have been tidied away by hand.
func (p partialCopy) At() string {
	if p.AtUnix == 0 {
		return ""
	}
	return time.Unix(p.AtUnix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// AsideSuffix renders the filename suffix the complete copy was renamed
// with, or "" when the value named none. Rendered as the literal suffix
// rather than as a bare number, because it is meant to be typed into an ls
// on the hypervisor.
func (p partialCopy) AsideSuffix() string {
	if p.AsideStamp == "" {
		return ""
	}
	return ".vmsync-replaced-" + p.AsideStamp
}
