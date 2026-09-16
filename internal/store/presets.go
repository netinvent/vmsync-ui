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
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Presets are the built-in transport profiles offered in the UI.
//
// These are canonical here rather than in the agent: this is where a person
// picks one, and what travels to the agent is the resolved profile, never a
// preset name. The agent keeps its own copy only as a fallback for an entry
// that arrives with an empty profile, so the two cannot disagree about what
// actually runs -- the fields on the wire always win.
var Presets = []Preset{
	{
		Name:  "wan",
		Label: "WAN link",
		Why:   "The link is the bottleneck, so heavier compression pays for itself. Buffering smooths the bursty read pattern a delta sync produces, and a deeper queue hides round-trip latency.",
		Profile: SyncProfile{
			Compress:      "zstd",
			CompressLevel: "5",
			NetBuffer:     "128k,1G",
			IODepth:       16,
		},
	},
	{
		Name:  "lan",
		Label: "LAN link",
		Why:   "The CPU is the bottleneck, not the wire. Compression stays as light as it goes while still removing the easy redundancy in qcow2 data.",
		Profile: SyncProfile{
			Compress:      "zstd",
			CompressLevel: "1",
			IODepth:       8,
		},
	},
	{
		Name:  "direct",
		Label: "No bridge",
		Why:   "No compression, no buffering, and no helper binary needed on the target. The floor to measure the others against, and the right answer on a link fast enough that any compression is a net loss.",
		Profile: SyncProfile{
			IODepth: 8,
		},
	},
}

// Preset is a named starting point for a profile.
type Preset struct {
	Name    string
	Label   string
	Why     string
	Profile SyncProfile
}

// PresetByName returns a preset's profile.
func PresetByName(name string) (SyncProfile, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p.Profile, true
		}
	}
	return SyncProfile{}, false
}

// ValidateProfile mirrors the agent's own validation.
//
// The agent validates again on receipt and is the authority -- it is the
// thing that builds a command line, and it must never trust this. Checking
// here as well is purely so a mistake is caught while the person who made
// it is still looking at the form, instead of surfacing minutes later in a
// hypervisor's journal as a skipped schedule entry.
func ValidateProfile(p SyncProfile) error {
	switch p.Compress {
	case "", "zstd", "s2":
	default:
		return fmt.Errorf("compression must be zstd, s2, or none (got %q)", p.Compress)
	}
	if p.Compress == "" && p.CompressLevel != "" {
		return fmt.Errorf("a compression level was given without an algorithm")
	}
	if p.Compress == "s2" {
		switch p.CompressLevel {
		case "", "default", "better", "best":
		default:
			return fmt.Errorf("s2 takes default, better or best -- not %q", p.CompressLevel)
		}
	}
	if p.Compress == "zstd" && p.CompressLevel != "" {
		var n int
		if _, err := fmt.Sscanf(p.CompressLevel, "%d", &n); err != nil || n < 1 || n > 19 {
			return fmt.Errorf("zstd takes a level from 1 to 19 -- not %q", p.CompressLevel)
		}
	}
	if p.UseSSH && p.Compress == "" && p.NetBuffer == "" {
		return fmt.Errorf("tunnelling over SSH has no effect without compression or buffering, which are what route traffic through the bridge")
	}
	if p.IODepth < 0 || p.IODepth > 64 {
		return fmt.Errorf("io depth must be between 1 and 64 (or 0 for the default)")
	}
	// Rejected here rather than passed through: this value travels into a
	// schedule document an agent decodes, and an agent that does not
	// recognise a verify mode refuses the WHOLE document -- so one bad field
	// on one VM stops that host's entire schedule, not just this entry.
	switch p.Verify {
	case "", VerifyFast, VerifyFull, VerifyQemuImg:
	default:
		return fmt.Errorf("verification must be %s, %s, %s, or none (got %q)",
			VerifyFast, VerifyFull, VerifyQemuImg, p.Verify)
	}
	// Same reasoning as the verify mode above, and the same consequence if it
	// slipped through: the agent rejects this combination, and it rejects the
	// whole schedule document when it does.
	if p.VerifyFailureReinit && p.Verify == "" {
		return fmt.Errorf("repairing a failed verification needs a verification mode -- without one there is nothing to fail")
	}
	if p.ReinitAfterFailures < 0 || p.ReinitAfterFailures > 100 {
		return fmt.Errorf("reinit-after-failures must be between 0 and 100")
	}
	if p.TargetDiskPath != "" && p.TargetDiskPath[0] != '/' {
		return fmt.Errorf("the target disk path must be absolute")
	}
	if p.TimestampToleranceSec < 0 || p.TimestampToleranceSec > 3600 {
		return fmt.Errorf("the timestamp tolerance must be between 0 and 3600 seconds -- past an hour it stops catching stray writes at all")
	}
	if p.Retention != "" {
		if err := validateRetention(p.Retention); err != nil {
			return err
		}
	}
	return nil
}

// validateRetention checks a "<count>,<interval>" retention spec.
//
// Duplicated from the engine's restorepoint.ParsePolicy rather than imported,
// like every other validation here: this is a separately-versioned program and
// the agent validates the same value again with the real parser. What this
// buys is the error arriving in the browser where it was typed, instead of as
// a refused schedule entry noticed days later.
//
// Worth being strict about. vmsync REFUSES a whole sync when -retention is set
// and the target filesystem cannot reflink -- deliberately, so nobody
// discovers months later that no restore point was ever taken -- so a typo
// here stops replication for that pair rather than degrading it.
func validateRetention(spec string) error {
	count, interval, ok := strings.Cut(spec, ",")
	if !ok {
		return fmt.Errorf("retention must be \"<count>,<interval>\", for example 24,3h (got %q)", spec)
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || n < 1 {
		return fmt.Errorf("retention count must be a whole number of copies to keep, at least 1 (got %q)", count)
	}
	d, err := time.ParseDuration(strings.TrimSpace(interval))
	if err != nil {
		return fmt.Errorf("retention interval must be a duration such as 3h or 90m (got %q)", interval)
	}
	if d < 0 {
		return fmt.Errorf("retention interval cannot be negative (got %q)", interval)
	}
	return nil
}

// The verification modes vmsync accepts. Kept here because this package is
// what the schedule form renders from and what validates a submission, and
// an unrecognised value reaches an agent that will refuse the whole schedule
// document.
//
// All three compare the target against the same frozen source snapshot the
// copy read from, so all three answer the same question. Only VerifyQemuImg
// suspends the source, and only to keep that snapshot's scratch space empty
// across a full-image read -- not because the comparison needs a stopped
// guest.
const (
	// VerifyFast stops at the first differing range. The right default for
	// a scheduled run.
	VerifyFast = "fast"
	// VerifyFull reports every differing range and the total bytes.
	VerifyFull = "full"
	// VerifyQemuImg uses qemu-img compare, an independent implementation --
	// the tie-breaker when another mode reports a mismatch. Suspends.
	VerifyQemuImg = "qemu-img"
)

// SuspendsSource reports whether a verification mode suspends the source VM.
//
// This is what the read-only role hinges on: offering "just a verification"
// to someone given the lesser role must not hand them a production-impacting
// action under a harmless-sounding name.
//
// Written as an explicit allowlist of the NON-suspending modes rather than a
// list of suspending ones, so the default for an unrecognised value is
// "assume it suspends". This function used to name the suspending modes, and
// the failure mode of getting that direction wrong is the bad one: when the
// modes were renamed, a stale list would have reported the new suspending
// mode as harmless and let a read-only account pause a production guest.
func SuspendsSource(verify string) bool {
	switch verify {
	case "", VerifyFast, VerifyFull:
		return false
	default:
		return true
	}
}
