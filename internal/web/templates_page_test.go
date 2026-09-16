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

	"vmsync-ui/internal/store"
)

func TestBuildTemplatesView(t *testing.T) {
	settings := store.Settings{Templates: map[string]store.ScheduleTemplate{
		"nightly": {Name: "nightly", IntervalSeconds: 3600, Enabled: true},
		"default": {Name: "default", IntervalSeconds: 900, Enabled: true},
		"archive": {Name: "archive", IntervalSeconds: 86400, Enabled: true},
	}}
	schedules := map[string][]store.ScheduleEntry{
		"a1": {
			{VM: "db01", Template: "nightly"},
			{VM: "web01", Template: "nightly"},
			{VM: "lab01"}, // no template
		},
	}
	v := BuildTemplatesView(settings, schedules)

	t.Run("the default sorts first", func(t *testing.T) {
		// It is the one that applies to VMs nobody listed, so it is the one to
		// read before the others.
		if len(v.Rows) != 3 || v.Rows[0].Name != store.DefaultTemplateName {
			t.Fatalf("rows = %v, want the default first", names(v.Rows))
		}
		if !v.Rows[0].IsDefault() {
			t.Error("the default row does not report itself as the default")
		}
		if !v.HasDefault {
			t.Error("HasDefault is false with a default template present")
		}
	})

	t.Run("usage is counted from entries that name it", func(t *testing.T) {
		for _, r := range v.Rows {
			switch r.Name {
			case "nightly":
				if len(r.UsedBy) != 2 || !r.InUse() {
					t.Errorf("nightly UsedBy = %v, want db01 and web01", r.UsedBy)
				}
			case "archive":
				if r.InUse() {
					t.Errorf("archive reports in use by %v", r.UsedBy)
				}
			}
		}
	})

	t.Run("no default means auto-coverage is off", func(t *testing.T) {
		// Its ABSENCE is the feature's off switch, and the page says so rather
		// than leaving an operator to infer it from a list that happens not to
		// contain a name.
		noDefault := store.Settings{Templates: map[string]store.ScheduleTemplate{
			"nightly": {Name: "nightly", IntervalSeconds: 3600},
		}}
		if BuildTemplatesView(noDefault, nil).HasDefault {
			t.Error("HasDefault is true with no default template")
		}
	})
}

func names(rows []TemplateRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// The structural rules this console CAN enforce. It cannot check the verify
// calendar's grammar -- that needs pkg/schedcal, which lives in the agent's
// module -- but publishing a template the agent will refuse breaks every VM
// inheriting it, so everything checkable is checked here.
func TestValidateTemplate(t *testing.T) {
	ok := store.ScheduleTemplate{Name: "nightly", IntervalSeconds: 3600, Enabled: true}

	for _, tc := range []struct {
		name     string
		tname    string
		tpl      func() store.ScheduleTemplate
		wantErr  bool
		mentions string
	}{
		{"a plain template", "nightly", func() store.ScheduleTemplate { return ok }, false, ""},
		{"no name", "", func() store.ScheduleTemplate { return ok }, true, "name"},
		{"no cadence", "nightly", func() store.ScheduleTemplate {
			t := ok
			t.IntervalSeconds = 0
			return t
		}, true, "interval"},
		{"a negative verify interval", "nightly", func() store.ScheduleTemplate {
			t := ok
			t.VerifyIntervalSeconds = -1
			return t
		}, true, "negative"},
		{"both cadence forms at once", "nightly", func() store.ScheduleTemplate {
			t := ok
			t.VerifyIntervalSeconds = 86400
			t.VerifyDays = "Sun"
			return t
		}, true, "one or the other"},
		{
			// The estate-wide-window shape: the template owns the window, each
			// entry names its own mode. Refusing this would forbid what
			// templates are most worth having.
			"a NON-default may carry the window alone", "nightly",
			func() store.ScheduleTemplate {
				t := ok
				t.VerifyDays = "Sun *-*-01..07"
				t.VerifyWindow = "02:00-12:00"
				return t
			}, false, "",
		},
		{
			// The default synthesises entries for VMs with none, and those have
			// no other source for a mode.
			"the default may not", store.DefaultTemplateName,
			func() store.ScheduleTemplate {
				t := ok
				t.Name = store.DefaultTemplateName
				t.VerifyDays = "Sun *-*-01..07"
				t.VerifyWindow = "02:00-12:00"
				return t
			}, true, "complete on its own",
		},
		{
			"the default WITH a mode is fine", store.DefaultTemplateName,
			func() store.ScheduleTemplate {
				t := ok
				t.Name = store.DefaultTemplateName
				t.VerifyDays = "Sun"
				t.Profile.Verify = store.VerifyFast
				return t
			}, false, "",
		},
		{
			// A bad profile in a template is a bad profile in every VM that
			// inherits it.
			"a retired verify mode", "nightly",
			func() store.ScheduleTemplate {
				t := ok
				t.Profile.Verify = "online"
				return t
			}, true, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidateTemplate(tc.tname, tc.tpl())
			if tc.wantErr && err == nil {
				t.Fatal("accepted a template the agent would refuse")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused a valid template: %v", err)
			}
			if tc.mentions != "" && err != nil && !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error %q does not mention %q, so it does not say what to change", err, tc.mentions)
			}
		})
	}
}

// The join this page exists for: a template filled in here has to come back
// out of the endpoint an agent polls, unresolved, so the AGENT resolves it.
func TestTemplateSaveReachesTheAgentConfig(t *testing.T) {
	s := testServer(t)

	form := url.Values{}
	form.Set("name", "nightly")
	form.Set("interval_minutes", "60")
	form.Set("preset", "lan")
	form.Set("verify", store.VerifyFast)
	form.Set("verify_days", "Sun *-*-01..07")
	form.Set("verify_window", "02:00-12:00")
	form.Set("enabled", "on")
	if rec := post(t, s, "/templates/save", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d, want a redirect: %s", rec.Code, rec.Body.String())
	}

	settings, err := s.Store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := settings.Templates["nightly"]
	if !ok {
		t.Fatalf("the template was not stored: %+v", settings.Templates)
	}
	if got.IntervalSeconds != 3600 {
		t.Errorf("IntervalSeconds = %d, want 3600", got.IntervalSeconds)
	}
	if got.VerifyDays != "Sun *-*-01..07" || got.VerifyWindow != "02:00-12:00" {
		t.Errorf("calendar = %q %q, want it stored verbatim", got.VerifyDays, got.VerifyWindow)
	}
	// Resolved into explicit profile fields, never a preset name: the agent
	// has never heard of "lan", and making it learn would tie the two
	// programs' vocabularies together forever.
	if got.Profile.Compress == "" {
		t.Error("the preset was not resolved into profile fields")
	}
	// The inner name comes from the key, so the two cannot disagree -- the
	// agent refuses a template whose name and key differ.
	if got.Name != "nightly" {
		t.Errorf("Name = %q, want it filled from the map key", got.Name)
	}
}

// Deleting a template that entries still name would strand them: the agent's
// resolver returns such an entry unchanged, it then fails its own validation,
// and the VM silently stops replicating.
func TestTemplateDeleteRefusesWhileEntriesNameIt(t *testing.T) {
	s := testServer(t)

	save := url.Values{}
	save.Set("name", "nightly")
	save.Set("interval_minutes", "60")
	save.Set("preset", "lan")
	if rec := post(t, s, "/templates/save", save); rec.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d", rec.Code)
	}
	if err := s.Store.SetScheduleEntry("a1", store.ScheduleEntry{
		VM: "db01", IntervalSeconds: 900, Enabled: true, Template: "nightly",
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, s, "/templates/delete", url.Values{"name": {"nightly"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete returned %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("delete of an in-use template did not report an error: %s", loc)
	}
	settings, _ := s.Store.Settings()
	if _, still := settings.Templates["nightly"]; !still {
		t.Error("an in-use template was deleted anyway, stranding every entry that names it")
	}

	// Unlink the VM and it deletes cleanly.
	if err := s.Store.SetScheduleEntry("a1", store.ScheduleEntry{
		VM: "db01", IntervalSeconds: 900, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "/templates/delete", url.Values{"name": {"nightly"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete returned %d", rec.Code)
	}
	settings, _ = s.Store.Settings()
	if _, still := settings.Templates["nightly"]; still {
		t.Error("the template survived a delete with nothing naming it")
	}
}

// A template the agent would refuse must not be publishable from here.
func TestTemplateSaveRefusesBothCadenceForms(t *testing.T) {
	s := testServer(t)
	form := url.Values{}
	form.Set("name", "nightly")
	form.Set("interval_minutes", "60")
	form.Set("preset", "lan")
	form.Set("verify", store.VerifyFast)
	form.Set("verify_interval_minutes", "1440")
	form.Set("verify_days", "Sun")

	rec := post(t, s, "/templates/save", form)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("a template with both cadence forms was accepted: %s", loc)
	}
	settings, _ := s.Store.Settings()
	if _, stored := settings.Templates["nightly"]; stored {
		t.Error("a template the agent refuses was published anyway")
	}
}

// The structural rules the schedule form can now enforce at authoring time,
// instead of leaving them for the agent to complain about on adoption.
func TestScheduleSaveRefusesABadCadence(t *testing.T) {
	s := testServer(t)

	base := func() url.Values {
		f := url.Values{}
		f.Set("agent_id", "a1")
		f.Set("vm", "web01")
		f.Set("interval_minutes", "15")
		f.Set("preset", "lan")
		f.Set("enabled", "on")
		return f
	}

	t.Run("both cadence forms on one entry", func(t *testing.T) {
		f := base()
		f.Set("verify", store.VerifyFast)
		f.Set("verify_interval_minutes", "1440")
		f.Set("verify_days", "Sun")
		rec := post(t, s, "/schedule/save", f)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
			t.Errorf("accepted an entry with both cadence forms: %s", loc)
		}
	})

	t.Run("a calendar with no verify mode anywhere", func(t *testing.T) {
		f := base()
		f.Set("verify_days", "Sun *-*-01..07")
		f.Set("verify_window", "02:00-12:00")
		rec := post(t, s, "/schedule/save", f)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
			t.Errorf("accepted a cadence with nothing to verify: %s", loc)
		}
	})

	t.Run("a calendar whose MODE comes from the template is fine", func(t *testing.T) {
		// The combination templates exist to allow: neither object is complete
		// on its own. Refusing it would forbid the useful shape.
		save := url.Values{}
		save.Set("name", "nightly")
		save.Set("interval_minutes", "60")
		save.Set("preset", "lan")
		save.Set("verify", store.VerifyFull)
		if rec := post(t, s, "/templates/save", save); rec.Code != http.StatusSeeOther {
			t.Fatalf("template save returned %d", rec.Code)
		}

		f := base()
		f.Set("template", "nightly")
		f.Set("verify_days", "Sun *-*-01..07")
		f.Set("verify_window", "02:00-12:00")
		rec := post(t, s, "/schedule/save", f)
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "error=") {
			t.Errorf("refused an entry whose verify mode comes from its template: %s", loc)
		}
	})
}
