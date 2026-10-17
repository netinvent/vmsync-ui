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

// Package version holds the single version string for the console.
//
// Its own package, like vmsync's, so that every place reporting a version
// reads the same constant: the --version flag, the startup log line, and
// the footer of every rendered page. A footer that could disagree with the
// binary would be worse than no footer at all.
//
// DELIBERATELY SEPARATE FROM vmsync's. The console is its own module and
// its own release: it talks to agents over a versioned HTTP contract, and
// the whole point of the contract is that the two ends can be upgraded
// independently. Agents report their own version to this console and it
// shows them on the agents page; this constant answers the other half of
// that question -- which console is doing the asking -- and an incident
// where the two have drifted is exactly when somebody needs both numbers
// at once without having to guess one of them.
package version

const Version = "0.50-2026101601-beta"
