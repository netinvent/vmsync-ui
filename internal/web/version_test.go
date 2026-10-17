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
	"strings"
	"testing"

	"vmsync-ui/internal/version"
)

// getPage is the GET counterpart of schedule_test.go's post helper: signed
// in, routed through the real mux, so what it returns is what a browser
// would be sent.
func getPage(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, _ := signIn(t, s)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// THE VERSION HAS TO BE ON THE PAGE, not only behind --version.
//
// Knowing which console is running is a question people ask while looking
// at the console, usually during an incident and usually because an
// agent's reported version on the agents page does not match what they
// expected. An answer that requires shell access to the UI host is not an
// answer at that moment.
//
// Every page, because the one somebody happens to be on is the one that
// has to carry it -- which is also why it comes from a template function
// rather than a field each view type has to remember to populate.
func TestEveryPageShowsTheConsoleVersion(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/", "/agents", "/schedule", "/templates", "/failover", "/audit"} {
		t.Run(path, func(t *testing.T) {
			rec := getPage(t, s, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), version.Version) {
				t.Errorf("GET %s does not show the console version (%s) anywhere on the page", path, version.Version)
			}
		})
	}
}

// ...and it must be the real constant, not a hardcoded string that drifts
// from the binary. A footer disagreeing with the build is worse than no
// footer: it answers the question wrongly, with confidence.
func TestTheFooterVersionComesFromTheVersionPackage(t *testing.T) {
	s := testServer(t)
	body := getPage(t, s, "/").Body.String()

	i := strings.Index(body, "vmsync-ui ")
	if i < 0 {
		t.Fatal("the footer does not name the console at all")
	}
	if !strings.Contains(body[i:], version.Version) {
		t.Errorf("the footer's version is not version.Version (%s); it has been hardcoded and will drift from the binary", version.Version)
	}
	// A version that is empty renders as a footer saying "vmsync-ui " with
	// nothing after it, which reads as a rendering bug rather than as the
	// missing constant it would be.
	if strings.TrimSpace(version.Version) == "" {
		t.Error("version.Version is empty")
	}
}
