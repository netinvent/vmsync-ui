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
	"sort"
	"strconv"

	"vmsync-ui/internal/store"
)

// TemplateRow is one schedule template, plus what the estate makes of it.
type TemplateRow struct {
	Name     string
	Template store.ScheduleTemplate
	// UsedBy is the VMs whose entries name this template explicitly. It does
	// NOT include the VMs a default template covers by synthesis: those have
	// no entry naming anything, which is the whole point of them, and the
	// agent is the only party that knows which they are.
	UsedBy []string
}

// IsDefault marks the one template that behaves differently: it covers every
// syncable VM with no entry of its own, and its absence is the feature's off
// switch.
func (r TemplateRow) IsDefault() bool { return r.Name == store.DefaultTemplateName }

// IntervalMinutes renders the sync cadence for a form field.
func (r TemplateRow) IntervalMinutes() int {
	if r.Template.IntervalSeconds <= 0 {
		return 15
	}
	return r.Template.IntervalSeconds / 60
}

// VerifyIntervalMinutes is blank when there is no interval cadence, because
// blank is a meaningful value here: it means "verify on every sync", which is
// what a profile naming a verify mode does when nothing says otherwise.
func (r TemplateRow) VerifyIntervalMinutes() string {
	if r.Template.VerifyIntervalSeconds <= 0 {
		return ""
	}
	return strconv.Itoa(r.Template.VerifyIntervalSeconds / 60)
}

// InUse reports whether deleting this template would strand an entry.
func (r TemplateRow) InUse() bool { return len(r.UsedBy) > 0 }

// TemplatesView is everything the templates page renders.
type TemplatesView struct {
	Rows    []TemplateRow
	Presets []store.Preset
	// HasDefault says whether auto-coverage is on at all. Surfaced because its
	// absence is the off switch, and "no VM is covered automatically" is a
	// state an operator should be able to see rather than infer from a list
	// that happens not to contain a particular name.
	HasDefault bool
}

// BuildTemplatesView correlates the stored templates with the entries that
// name them.
func BuildTemplatesView(settings store.Settings, schedules map[string][]store.ScheduleEntry) TemplatesView {
	usedBy := map[string][]string{}
	for _, entries := range schedules {
		for _, e := range entries {
			if e.Template != "" {
				usedBy[e.Template] = append(usedBy[e.Template], e.VM)
			}
		}
	}

	v := TemplatesView{Presets: store.Presets}
	for name, t := range settings.Templates {
		vms := usedBy[name]
		sort.Strings(vms)
		v.Rows = append(v.Rows, TemplateRow{Name: name, Template: t, UsedBy: vms})
		if name == store.DefaultTemplateName {
			v.HasDefault = true
		}
	}
	// The default first, then alphabetically: it is the one that applies to
	// VMs nobody listed, so it is the one to read before the others.
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].IsDefault() != v.Rows[j].IsDefault() {
			return v.Rows[i].IsDefault()
		}
		return v.Rows[i].Name < v.Rows[j].Name
	})
	return v
}
