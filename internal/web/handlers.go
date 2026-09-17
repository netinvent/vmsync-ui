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

// Package web serves the operator-facing pages.
//
// Server-rendered html/template with no build step and no framework. This
// is an operations console read during incidents; the fewer moving parts
// between "the agent reported" and "a human can see it", the better.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vmsync-ui/internal/auth"
	"vmsync-ui/internal/store"
)

//go:embed templates/*.html static/* static/fonts/*
var assets embed.FS

type Server struct {
	Store   *store.Store
	Auth    *auth.Manager
	Log     *slog.Logger
	Secure  bool   // set the Secure flag on cookies; false only for plain-HTTP local testing
	Grafana string // optional deep link for trends, which this UI deliberately does not draw

	tpl *template.Template
}

func New(st *store.Store, am *auth.Manager, log *slog.Logger, secure bool, grafana string) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"statusClass": statusClass,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{Store: st, Auth: am, Log: log, Secure: secure, Grafana: grafana, tpl: tpl}, nil
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)

	mux.HandleFunc("GET /{$}", s.requireUser(s.getDashboard))
	mux.HandleFunc("GET /agents", s.requireUser(s.getAgents))
	mux.HandleFunc("GET /schedule", s.requireUser(s.getSchedule))
	mux.HandleFunc("GET /templates", s.requireUser(s.getTemplates))
	mux.HandleFunc("GET /failover", s.requireUser(s.getFailover))
	mux.HandleFunc("GET /audit", s.requireUser(s.getAudit))

	mux.HandleFunc("POST /agents/enrolment-token", s.requireAdmin(s.postEnrolmentToken))
	mux.HandleFunc("POST /agents/{id}/revoke", s.requireAdmin(s.postRevoke))
	mux.HandleFunc("POST /schedule/save", s.requireAdmin(s.postScheduleSave))
	mux.HandleFunc("POST /schedule/delete", s.requireAdmin(s.postScheduleDelete))
	mux.HandleFunc("POST /schedule/settings", s.requireAdmin(s.postSettings))
	mux.HandleFunc("POST /templates/save", s.requireAdmin(s.postTemplateSave))
	mux.HandleFunc("POST /templates/delete", s.requireAdmin(s.postTemplateDelete))
	mux.HandleFunc("POST /failover/operation", s.requireAdmin(s.postFailoverOperation))
	mux.HandleFunc("POST /failover/cancel", s.requireAdmin(s.postFailoverCancel))
}

// loadFleet reads every agent and its latest report, which nearly every
// page needs.
func (s *Server) loadFleet() ([]store.Agent, map[string]store.Report, error) {
	agents, err := s.Store.Agents()
	if err != nil {
		return nil, nil, err
	}
	reports := map[string]store.Report{}
	for _, a := range agents {
		if rep, ok, err := s.Store.Report(a.ID); err == nil && ok {
			reports[a.ID] = rep
		}
	}
	return agents, reports, nil
}

// --- page data ------------------------------------------------------------

type pageData struct {
	User    auth.User
	Active  string
	Grafana string
	Flash   string
	Error   string

	Dashboard Dashboard
	Schedule  ScheduleView
	Failover  FailoverView
	Runs      []RunView
	Agents    []AgentView
	Audit     []store.AuditEntry
	Templates TemplatesView
	NewToken  string
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This page renders only its own assets and never loads anything
	// external, so the policy can be as tight as it goes.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("render failed", "template", name, "error", err)
	}
}

// --- auth plumbing --------------------------------------------------------

func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.Auth.FromRequest(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, user)
	}
}

func (s *Server) requireAdmin(h func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request, user auth.User) {
		if !user.IsAdmin() {
			// 403 rather than a redirect: this is a deliberate refusal, and
			// bouncing to a page they can see would hide that.
			http.Error(w, "this action requires an admin account", http.StatusForbidden)
			return
		}
		if !user.CheckCSRF(r.FormValue("csrf")) {
			s.Log.Warn("rejected a form submission with a bad CSRF token", "actor", user.Username, "path", r.URL.Path)
			http.Error(w, "invalid form token; reload the page and try again", http.StatusBadRequest)
			return
		}
		h(w, r, user)
	})
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Auth.FromRequest(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", pageData{Active: "login"})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	username, password := r.FormValue("username"), r.FormValue("password")
	token, user, ok := s.Auth.SignIn(username, password)
	if !ok {
		// One message for both wrong-username and wrong-password: telling
		// them apart only helps someone enumerating accounts.
		s.Log.Warn("failed sign-in", "username", username, "remote", r.RemoteAddr)
		s.render(w, "login.html", pageData{Active: "login", Error: "Incorrect username or password."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	s.Log.Info("signed in", "actor", user.Username, "role", user.Role)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		s.Auth.SignOut(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- pages ----------------------------------------------------------------

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request, user auth.User) {
	agents, reports, err := s.loadFleet()
	if err != nil {
		s.Log.Error("could not read agents", "error", err)
		http.Error(w, "could not read state", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	s.render(w, "dashboard.html", pageData{
		User:      user,
		Active:    "dashboard",
		Grafana:   s.Grafana,
		Dashboard: BuildDashboard(agents, reports, now),
		// Enough to answer "did the last few syncs work", not a log: the
		// pairs table above already says whether replication is current,
		// so this only has to show what has been happening lately.
		Runs: BuildRunView(agents, reports, now, 15),
	})
}

func (s *Server) getAgents(w http.ResponseWriter, r *http.Request, user auth.User) {
	agents, reports, err := s.loadFleet()
	if err != nil {
		http.Error(w, "could not read state", http.StatusInternalServerError)
		return
	}
	s.render(w, "agents.html", pageData{
		User:     user,
		Active:   "agents",
		Agents:   BuildDashboard(agents, reports, time.Now()).Agents,
		NewToken: r.URL.Query().Get("token"),
		Flash:    r.URL.Query().Get("flash"),
	})
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, user auth.User) {
	agents, reports, err := s.loadFleet()
	if err != nil {
		http.Error(w, "could not read state", http.StatusInternalServerError)
		return
	}
	schedules, err := s.Store.Schedules()
	if err != nil {
		http.Error(w, "could not read the schedule", http.StatusInternalServerError)
		return
	}
	settings, err := s.Store.Settings()
	if err != nil {
		http.Error(w, "could not read settings", http.StatusInternalServerError)
		return
	}
	s.render(w, "schedule.html", pageData{
		User:     user,
		Active:   "schedule",
		Schedule: BuildScheduleView(agents, reports, schedules, settings, time.Now()),
		Flash:    r.URL.Query().Get("flash"),
		Error:    r.URL.Query().Get("error"),
	})
}

// postScheduleSave adds or updates one VM's entry.
//
// The profile is resolved from a preset here, in the UI, and travels to the
// agent as explicit fields -- never as a preset name. That keeps the agent
// from having to agree with this program about what "wan" means, and means
// what an operator sees on this page is exactly what will run.
func (s *Server) postScheduleSave(w http.ResponseWriter, r *http.Request, user auth.User) {
	agentID := r.FormValue("agent_id")
	vm := r.FormValue("vm")
	if agentID == "" || vm == "" {
		http.Error(w, "agent_id and vm are required", http.StatusBadRequest)
		return
	}

	presetName := r.FormValue("preset")
	profile, ok := store.PresetByName(presetName)
	if !ok {
		s.redirectSchedule(w, r, "", "Unknown profile "+presetName+".")
		return
	}
	profile.Verify = r.FormValue("verify")
	// Offered in the form where NoChecksum below is not, and the asymmetry is
	// the point: this one only ever adds a check, while that one removes one.
	// Left to ValidateProfile to refuse without a verify mode, rather than
	// silently dropped here -- an operator who picked "recopy once" and got
	// "report it and stop" without being told has been lied to about what
	// their schedule does.
	profile.VerifyFailureReinit = r.FormValue("verify_failure_reinit") != ""
	profile.TargetDiskPath = strings.TrimSpace(r.FormValue("target_disk_path"))
	profile.Retention = strings.TrimSpace(r.FormValue("retention"))
	// Blank means 0, which is vmsync's own default and an exact comparison.
	profile.TimestampToleranceSec, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("timestamp_tolerance_sec")))

	// NoChecksum is deliberately NOT overridable from this form, and that is
	// a decision rather than an omission.
	//
	// It disables vmsync's pre-commit integrity check, which is on by
	// default. Nobody needs the flag to cope with a target that has no
	// vmsync-bridge-helper: vmsync already skips the check there with a
	// warning rather than failing the sync. So the only thing a form
	// checkbox would buy is a one-click way to silence the warning that says
	// an integrity check is not running -- which is the last warning an
	// estate should be able to switch off casually.
	//
	// It stays a profile field so it round-trips through a preset and
	// through the agent's own JSON, where turning it off is a deliberate,
	// reviewable act.

	// The one capability split that is easy to get wrong by accident. Which
	// modes suspend is store.SuspendsSource's call, not this handler's.
	// Only an admin reaches this handler at all, but checking here keeps the
	// rule in one place for when an operator role is added.
	if store.SuspendsSource(profile.Verify) && !user.MayRunSuspendingVerify() {
		s.redirectSchedule(w, r, "", "That verification mode suspends the source VM and needs an admin account.")
		return
	}
	if err := store.ValidateProfile(profile); err != nil {
		s.redirectSchedule(w, r, "", err.Error())
		return
	}

	minutes, err := strconv.Atoi(r.FormValue("interval_minutes"))
	if err != nil || minutes < 1 || minutes > 10080 {
		s.redirectSchedule(w, r, "", "The interval must be between 1 minute and 7 days.")
		return
	}

	// How often a sync should ALSO verify. Blank means every run, which is
	// what happened before this field existed and is still what a profile
	// naming a verify mode gets by default.
	//
	// A cadence with no mode is refused rather than ignored: it reads as
	// "verify this often" and would do nothing at all, and discovering that
	// is the sort of thing that happens after somebody trusted it for a
	// month. The agent is fail-safe about the same combination -- no mode
	// means no verify -- but silence there is a property of the executor, not
	// a reason for the console to accept a request it cannot honour.
	verifyMinutes := 0
	if raw := strings.TrimSpace(r.FormValue("verify_interval_minutes")); raw != "" {
		verifyMinutes, err = strconv.Atoi(raw)
		if err != nil || verifyMinutes < 1 || verifyMinutes > 10080 {
			s.redirectSchedule(w, r, "", "The verification interval must be between 1 minute and 7 days, or blank to verify on every sync.")
			return
		}
		if profile.Verify == "" {
			s.redirectSchedule(w, r, "", "A verification interval needs a verification mode: it says how often to verify, not whether to.")
			return
		}
		if verifyMinutes < minutes {
			// Not an error -- it resolves to "every sync", which is a
			// coherent thing to want -- but it is almost always a mistake,
			// and the resolution is invisible without being told.
			s.redirectSchedule(w, r, "", fmt.Sprintf("Saved, but the verification interval (%dm) is shorter than the sync interval (%dm), so every sync will verify.", verifyMinutes, minutes))
		}
	}

	// Blank means inherit the estate default, which is the common case and
	// must not be an error.
	shutdownSec := 0
	if raw := strings.TrimSpace(r.FormValue("shutdown_timeout_sec")); raw != "" {
		shutdownSec, err = strconv.Atoi(raw)
		if err != nil {
			s.redirectSchedule(w, r, "", "The shutdown timeout must be a whole number of seconds, or blank to inherit.")
			return
		}
		if err := store.ValidateShutdownTimeout(shutdownSec); err != nil {
			s.redirectSchedule(w, r, "", err.Error())
			return
		}
	}

	entry := store.ScheduleEntry{
		VM:              vm,
		IntervalSeconds: minutes * 60,
		// A checkbox sends nothing at all when unticked; browsers send "on"
		// when ticked, but that value is convention rather than guarantee,
		// so presence is what is actually being tested here.
		Enabled:            r.FormValue("enabled") != "",
		Profile:            profile,
		TargetHost:         strings.TrimSpace(r.FormValue("target_host")),
		ShutdownTimeoutSec: shutdownSec,
		// 0 means "verify on every sync", which is what a profile naming a
		// verify mode has always done.
		VerifyIntervalSeconds: verifyMinutes * 60,
		Preset:                presetName,
		// The form owns these now, so an empty value is a choice rather than
		// "leave whatever is stored alone". That is what retired
		// carryUneditedFields: it existed only to protect fields this page
		// could not edit, and there are none left.
		Template:     strings.TrimSpace(r.FormValue("template")),
		VerifyDays:   strings.TrimSpace(r.FormValue("verify_days")),
		VerifyWindow: strings.TrimSpace(r.FormValue("verify_window")),
	}

	// The structural half of what the agent checks, at authoring time rather
	// than on adoption. The calendar's GRAMMAR still cannot be checked here --
	// that needs pkg/schedcal, in the agent's module -- so a malformed
	// expression still reaches the agent, which names it in Complaints() and
	// shows it back on this page.
	templates, err := s.Store.Settings()
	if err != nil {
		http.Error(w, "could not read settings", http.StatusInternalServerError)
		return
	}
	if err := store.ValidateEntryCadence(entry, templates.Templates); err != nil {
		s.redirectSchedule(w, r, "", err.Error())
		return
	}

	detail := fmt.Sprintf("every %dm, %s profile, enabled=%v", minutes, presetName, entry.Enabled)
	if shutdownSec > 0 {
		detail += fmt.Sprintf(", shutdown_timeout=%ds", shutdownSec)
	}
	entryID, err := s.Store.AppendAudit(user.Username, "set-schedule", vm, detail)
	if err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetScheduleEntry(agentID, entry); err != nil {
		_ = s.Store.CompleteAudit(entryID, "failed: "+err.Error())
		s.redirectSchedule(w, r, "", "Could not save: "+err.Error())
		return
	}
	_ = s.Store.CompleteAudit(entryID, "saved")
	s.Log.Info("schedule entry saved", "actor", user.Username, "vm", vm, "interval_m", minutes, "preset", presetName, "enabled", entry.Enabled)

	s.redirectSchedule(w, r, "Saved. The agent picks this up on its next poll, usually within seconds.", "")
}

func (s *Server) postScheduleDelete(w http.ResponseWriter, r *http.Request, user auth.User) {
	agentID, vm := r.FormValue("agent_id"), r.FormValue("vm")
	if agentID == "" || vm == "" {
		http.Error(w, "agent_id and vm are required", http.StatusBadRequest)
		return
	}
	entryID, err := s.Store.AppendAudit(user.Username, "remove-schedule", vm, "")
	if err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	if err := s.Store.DeleteScheduleEntry(agentID, vm); err != nil {
		_ = s.Store.CompleteAudit(entryID, "failed: "+err.Error())
		s.redirectSchedule(w, r, "", "Could not remove: "+err.Error())
		return
	}
	_ = s.Store.CompleteAudit(entryID, "removed")
	s.Log.Info("schedule entry removed", "actor", user.Username, "vm", vm)
	s.redirectSchedule(w, r, "Removed from the schedule. This does not pause replication at the VM level -- use a replication role for that.", "")
}

// postSettings saves the estate-wide defaults.
//
// Only the two an operator has a reason to change from here. TargetReplicationSlots
// is a map with a row per host and belongs to a form of its own, and the
// report/poll intervals are protocol timing rather than policy -- exposing
// either alongside these would invite changing something structural while
// meaning to adjust a timeout.
func (s *Server) postSettings(w http.ResponseWriter, r *http.Request, user auth.User) {
	current, err := s.Store.Settings()
	if err != nil {
		http.Error(w, "could not read settings", http.StatusInternalServerError)
		return
	}

	shutdownSec, err := strconv.Atoi(strings.TrimSpace(r.FormValue("shutdown_timeout_sec")))
	if err != nil {
		s.redirectSchedule(w, r, "", "The shutdown timeout must be a whole number of seconds.")
		return
	}
	// 0 means "inherit" on a VM, but the estate default is what there is to
	// inherit FROM -- so here it has to be a real number.
	if shutdownSec == 0 {
		s.redirectSchedule(w, r, "", "The estate default is what a VM inherits, so it needs a real value.")
		return
	}
	if err := store.ValidateShutdownTimeout(shutdownSec); err != nil {
		s.redirectSchedule(w, r, "", err.Error())
		return
	}

	maxConcurrent, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_concurrent_syncs")))
	if err != nil || maxConcurrent < 1 || maxConcurrent > 128 {
		s.redirectSchedule(w, r, "", "Concurrent syncs must be between 1 and 128.")
		return
	}

	current.ShutdownTimeoutSec = shutdownSec
	current.MaxConcurrentSyncs = maxConcurrent

	detail := fmt.Sprintf("shutdown_timeout=%ds max_concurrent=%d", shutdownSec, maxConcurrent)
	auditID, err := s.Store.AppendAudit(user.Username, "set-estate-defaults", "", detail)
	if err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetSettings(current); err != nil {
		_ = s.Store.CompleteAudit(auditID, "failed: "+err.Error())
		s.redirectSchedule(w, r, "", "Could not save: "+err.Error())
		return
	}
	_ = s.Store.CompleteAudit(auditID, "saved")
	s.Log.Info("estate defaults saved", "actor", user.Username,
		"shutdown_timeout_sec", shutdownSec, "max_concurrent_syncs", maxConcurrent)

	s.redirectSchedule(w, r, "Saved. Agents pick this up on their next poll.", "")
}

func (s *Server) redirectSchedule(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	q := url.Values{}
	if flash != "" {
		q.Set("flash", flash)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	http.Redirect(w, r, "/schedule?"+q.Encode(), http.StatusSeeOther)
}

// --- failover ------------------------------------------------------------

func (s *Server) getFailover(w http.ResponseWriter, r *http.Request, user auth.User) {
	agents, reports, err := s.loadFleet()
	if err != nil {
		http.Error(w, "could not read state", http.StatusInternalServerError)
		return
	}
	ops, err := s.Store.Operations()
	if err != nil {
		http.Error(w, "could not read operations", http.StatusInternalServerError)
		return
	}
	s.render(w, "failover.html", pageData{
		User:     user,
		Active:   "failover",
		Failover: BuildFailoverView(agents, reports, ops, time.Now()),
		Flash:    r.URL.Query().Get("flash"),
		Error:    r.URL.Query().Get("error"),
	})
}

// rolesSettableByHand is what -update-role may be asked for from this page.
//
// `promoted` is deliberately absent. Promotion is an operation with real
// preconditions -- vmsync checks that a usable replica actually exists and
// reports the data-loss window it accepts -- and writing the role directly
// would reach the same recorded state having verified none of it. An
// operator wanting a promotion has a button for it three columns to the
// left; one that types this instead should be refused, not accommodated.
var rolesSettableByHand = map[string]string{
	store.RoleTarget: "a normal replication target again",
	store.RoleSource: "the primary of its pair",
	store.RolePaused: "replication suspended",
}

// postFailoverOperation issues one operation to one agent.
//
// Every kind goes through here rather than through a route each, because
// what differs between them is which fields are required and which agent
// runs it -- and gathering that in one switch makes the differences legible
// side by side instead of spread across five near-identical handlers.
func (s *Server) postFailoverOperation(w http.ResponseWriter, r *http.Request, user auth.User) {
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	vm := strings.TrimSpace(r.FormValue("vm"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	if agentID == "" || vm == "" {
		http.Error(w, "agent_id and vm are required", http.StatusBadRequest)
		return
	}

	op := store.Operation{
		Kind:      kind,
		VM:        vm,
		CreatedBy: user.Username,
		// The peer travels as a CLAIM, checked by the agent against the VM's
		// own libvirt metadata and refused on a mismatch. It is never used as
		// an endpoint. See Operation.PeerHost.
		PeerHost: strings.TrimSpace(r.FormValue("peer_host")),
		PeerVM:   strings.TrimSpace(r.FormValue("peer_vm")),
	}

	var detail string
	switch kind {
	case store.OpPromote:
		mode := r.FormValue("mode")
		if mode != "planned" && mode != "forced" {
			s.redirectFailover(w, r, "", "Choose a promotion mode: planned or forced.")
			return
		}
		op.Mode = mode
		// A checkbox sends nothing when unticked; presence is what is
		// actually being tested, not the "on" browsers conventionally send.
		op.StartVM = r.FormValue("start") != ""
		op.Force = r.FormValue("force") != ""
		op.ArmFence = r.FormValue("arm_fence") != ""
		detail = fmt.Sprintf("mode=%s start=%v force=%v fence=%v", mode, op.StartVM, op.Force, op.ArmFence)

	case store.OpInvert:
		if op.PeerHost == "" {
			s.redirectFailover(w, r, "", "An inversion needs the promoted peer, and this row does not name one.")
			return
		}
		detail = "peer=" + op.PeerHost + ":" + op.PeerVM

	case store.OpShutdown:
		// No peer to check: it acts on the named domain, on its own host.
		// It does need to know how long to wait, though, and that is
		// resolved HERE rather than by the agent -- see
		// Operation.ShutdownTimeoutSec for why a decision must not depend on
		// what a setting happened to say when it was eventually executed.
		sec, err := s.resolveShutdownTimeout(agentID, vm)
		if err != nil {
			http.Error(w, "could not read the schedule", http.StatusInternalServerError)
			return
		}
		op.ShutdownTimeoutSec = sec
		detail = fmt.Sprintf("shutdown_timeout=%ds", sec)

	case store.OpSetRole:
		role := strings.TrimSpace(r.FormValue("role"))
		if _, ok := rolesSettableByHand[role]; !ok {
			s.redirectFailover(w, r, "",
				"That role cannot be set by hand. To make a replica serve live, use Promote — it verifies the replica first and reports what the failover costs.")
			return
		}
		op.Mode = role
		detail = "role=" + role
		// A role change is about this domain alone, and passing a peer would
		// have the agent check a relationship the operation does not use.
		op.PeerHost, op.PeerVM = "", ""

	case store.OpRestore:
		tag := strings.TrimSpace(r.FormValue("tag"))
		if tag == "" {
			s.redirectFailover(w, r, "", "A restore needs a restore point. Pick one from the list.")
			return
		}
		// Checked against what that agent last reported rather than accepted
		// on trust, so a tag typed by hand, or one from a page left open
		// while retention pruned it, is refused HERE -- where the operator
		// can see why -- instead of becoming an operation whose ID is burned
		// and whose failure they read in a log tail. The agent re-checks it
		// against the filesystem anyway; this is the half that can explain
		// itself.
		known, err := s.restorePointKnown(agentID, vm, tag)
		if err != nil {
			http.Error(w, "could not read the agent's last report", http.StatusInternalServerError)
			return
		}
		if !known {
			s.redirectFailover(w, r, "",
				"That restore point is not in "+vm+"'s latest report. It may have been pruned by retention since this page was loaded — reload and pick again.")
			return
		}
		op.Tag = tag
		detail = "tag=" + tag
		// The peer is deliberately KEPT, unlike a role change. It is not used
		// as an endpoint -- a restore acts on this domain alone -- but the
		// agent checks it against the replica's own replica_source, so the
		// UI's belief about which pair this is gets verified before anything
		// is overwritten. It is also what lets the follow-up disable the
		// source's schedule, so the source stops syncing into a replica that
		// has just been rolled back.

	case store.OpReinit, store.OpForceClean:
		// The second half of going back to replicating after a restore, and
		// the only kinds here that run on the SOURCE's agent -- they are
		// syncs, so they need that pair's whole transport configuration,
		// which lives on the schedule the source's agent holds.
		//
		// No peer claim: the agent resolves the target from the VM's own
		// replica_targets, the same as a scheduled run.
		op.PeerHost, op.PeerVM = "", ""
		detail = "full resync"
		if kind == store.OpForceClean {
			detail = "full resync, removing the target domain"
		}

	default:
		http.Error(w, "unknown operation kind", http.StatusBadRequest)
		return
	}

	// Intent first, outcome after -- the same order the store uses for the
	// operation itself, and for the same reason: a half-completed failover
	// is precisely the case that most needs attribution.
	auditID, err := s.Store.AppendAudit(user.Username, kind, vm, detail)
	if err != nil {
		s.Log.Error("could not write audit entry", "error", err)
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}

	rec, err := s.Store.CreateOperation(agentID, auditID, op, time.Now())
	if err != nil {
		_ = s.Store.CompleteAudit(auditID, "failed: "+err.Error())
		s.redirectFailover(w, r, "", err.Error())
		return
	}
	s.Log.Info("operation issued", "actor", user.Username, "kind", kind, "vm", vm,
		"agent_id", agentID, "operation_id", rec.ID, "detail", detail)

	s.redirectFailover(w, r, fmt.Sprintf(
		"Issued: %s on %s. The agent picks it up on its next poll, usually within seconds; it expires in %d minutes if nothing collects it.",
		kind, vm, int(store.OperationTTL.Minutes())), "")
}

// restorePointKnown reports whether an agent's latest report lists this tag
// for this VM.
//
// A restore is the one operation whose parameter is not a domain name or a
// role but a directory on a remote filesystem, so it is the one that can name
// something that has ceased to exist between the page rendering and the button
// being pressed -- retention prunes on every sync. Refusing here costs a
// reload; accepting would burn an operation ID permanently, because a kind
// that reaches the agent and fails is recorded refused and can never be
// retried with the same ID.
func (s *Server) restorePointKnown(agentID, vm, tag string) (bool, error) {
	report, ok, err := s.Store.Report(agentID)
	if err != nil {
		return false, err
	}
	if !ok {
		// No report at all from this agent. Not an error -- a freshly
		// enrolled agent looks like this -- but it certainly has not told us
		// about a restore point.
		return false, nil
	}
	for _, d := range report.Domains {
		if !strings.EqualFold(d.Name, vm) {
			continue
		}
		for _, rp := range d.RestorePoints {
			if rp.Tag == tag {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

// resolveShutdownTimeout finds the timeout for one VM: its own schedule
// entry's value, else the estate default, else vmsync's.
func (s *Server) resolveShutdownTimeout(agentID, vm string) (int, error) {
	settings, err := s.Store.Settings()
	if err != nil {
		return 0, err
	}
	schedules, err := s.Store.Schedules()
	if err != nil {
		return 0, err
	}
	var perVM int
	for _, e := range schedules[agentID] {
		if strings.EqualFold(e.VM, vm) {
			perVM = e.ShutdownTimeoutSec
			break
		}
	}
	return store.ResolveShutdownTimeout(perVM, settings.ShutdownTimeoutSec), nil
}

func (s *Server) postFailoverCancel(w http.ResponseWriter, r *http.Request, user auth.User) {
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "an operation id is required", http.StatusBadRequest)
		return
	}
	auditID, err := s.Store.AppendAudit(user.Username, "cancel-operation", id, "")
	if err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	if err := s.Store.CancelOperation(id, user.Username, time.Now()); err != nil {
		_ = s.Store.CompleteAudit(auditID, "failed: "+err.Error())
		s.redirectFailover(w, r, "", err.Error())
		return
	}
	_ = s.Store.CompleteAudit(auditID, "cancelled")
	s.Log.Info("operation cancelled", "actor", user.Username, "operation_id", id)
	s.redirectFailover(w, r,
		"Cancelled. If the agent had already started it, this stops it being published but does not undo what ran.", "")
}

func (s *Server) redirectFailover(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	q := url.Values{}
	if flash != "" {
		q.Set("flash", flash)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	http.Redirect(w, r, "/failover?"+q.Encode(), http.StatusSeeOther)
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request, user auth.User) {
	entries, err := s.Store.Audit()
	if err != nil {
		http.Error(w, "could not read the audit log", http.StatusInternalServerError)
		return
	}
	s.render(w, "audit.html", pageData{User: user, Active: "audit", Audit: entries})
}

// --- admin actions --------------------------------------------------------

func (s *Server) postEnrolmentToken(w http.ResponseWriter, r *http.Request, user auth.User) {
	hostname := r.FormValue("hostname")
	if hostname == "" {
		http.Error(w, "a hostname is required", http.StatusBadRequest)
		return
	}

	// Intent first, outcome after: a half-completed action is exactly the
	// case that most needs attribution, and recording only on success loses
	// precisely those.
	entryID, err := s.Store.AppendAudit(user.Username, "create-enrolment-token", hostname, "")
	if err != nil {
		s.Log.Error("could not write audit entry", "error", err)
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}

	token, err := s.Store.CreateEnrolmentToken(hostname, user.Username, 24*time.Hour)
	if err != nil {
		_ = s.Store.CompleteAudit(entryID, "failed: "+err.Error())
		http.Error(w, "could not create the token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.Store.CompleteAudit(entryID, "created")
	s.Log.Info("enrolment token created", "actor", user.Username, "hostname", hostname)

	// The clear token travels back in the redirect so it can be shown once.
	// It is single-use and expires in 24h, which is what makes that
	// acceptable -- but it is also why the page tells the operator this is
	// the only time they will see it.
	http.Redirect(w, r, "/agents?token="+token, http.StatusSeeOther)
}

func (s *Server) postRevoke(w http.ResponseWriter, r *http.Request, user auth.User) {
	id := r.PathValue("id")
	entryID, err := s.Store.AppendAudit(user.Username, "revoke-agent", id, "")
	if err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	if err := s.Store.Revoke(id); err != nil {
		_ = s.Store.CompleteAudit(entryID, "failed: "+err.Error())
		http.Error(w, "could not revoke: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.Store.CompleteAudit(entryID, "revoked")
	s.Log.Info("agent revoked", "actor", user.Username, "agent_id", id)
	http.Redirect(w, r, "/agents?flash=Agent+revoked.+It+can+no+longer+connect.", http.StatusSeeOther)
}

// statusClass maps a status to its CSS class. Administrative states get
// their own treatment rather than sharing the warning colour: a planned
// failover must not make the board look broken, or people learn to ignore
// the colour that means something is.
func statusClass(status string) string {
	switch status {
	case "ok":
		return "s-ok"
	case "warning":
		return "s-warn"
	case "critical":
		return "s-crit"
	case store.RolePromoted, store.RolePaused:
		return "s-admin"
	default:
		return "s-none"
	}
}

// --- schedule templates ---------------------------------------------------

func (s *Server) getTemplates(w http.ResponseWriter, r *http.Request, user auth.User) {
	settings, err := s.Store.Settings()
	if err != nil {
		http.Error(w, "could not read settings", http.StatusInternalServerError)
		return
	}
	schedules, err := s.Store.Schedules()
	if err != nil {
		http.Error(w, "could not read the schedule", http.StatusInternalServerError)
		return
	}
	s.render(w, "templates.html", pageData{
		User:      user,
		Active:    "templates",
		Templates: BuildTemplatesView(settings, schedules),
		Flash:     r.URL.Query().Get("flash"),
		Error:     r.URL.Query().Get("error"),
	})
}

func (s *Server) redirectTemplates(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	q := url.Values{}
	if flash != "" {
		q.Set("flash", flash)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	http.Redirect(w, r, "/templates?"+q.Encode(), http.StatusSeeOther)
}

func (s *Server) postTemplateSave(w http.ResponseWriter, r *http.Request, user auth.User) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.redirectTemplates(w, r, "", "A template needs a name.")
		return
	}

	minutes, err := strconv.Atoi(strings.TrimSpace(r.FormValue("interval_minutes")))
	if err != nil || minutes <= 0 {
		s.redirectTemplates(w, r, "", "The sync interval must be a whole number of minutes, greater than zero.")
		return
	}

	presetName := r.FormValue("preset")
	profile, ok := store.PresetByName(presetName)
	if !ok {
		s.redirectTemplates(w, r, "", "Unknown profile preset.")
		return
	}
	profile.Verify = r.FormValue("verify")
	profile.VerifyFailureReinit = r.FormValue("verify_failure_reinit") != ""
	profile.TargetDiskPath = strings.TrimSpace(r.FormValue("target_disk_path"))
	profile.Retention = strings.TrimSpace(r.FormValue("retention"))

	// A suspending verify mode is the one profile choice that stops a
	// production guest, so it carries the same permission here as on the
	// schedule form -- a template applies it to every VM that inherits it,
	// which makes it more consequential, not less.
	if store.SuspendsSource(profile.Verify) && !user.MayRunSuspendingVerify() {
		s.redirectTemplates(w, r, "", "Your account may not select a verify mode that suspends the source.")
		return
	}

	t := store.ScheduleTemplate{
		Name:            name,
		IntervalSeconds: minutes * 60,
		Enabled:         r.FormValue("enabled") != "",
		Profile:         profile,
		Preset:          presetName,
		VerifyDays:      strings.TrimSpace(r.FormValue("verify_days")),
		VerifyWindow:    strings.TrimSpace(r.FormValue("verify_window")),
	}
	if raw := strings.TrimSpace(r.FormValue("verify_interval_minutes")); raw != "" {
		vm, err := strconv.Atoi(raw)
		if err != nil || vm <= 0 {
			s.redirectTemplates(w, r, "", "The verify interval must be a whole number of minutes, greater than zero — or blank to verify on every sync.")
			return
		}
		t.VerifyIntervalSeconds = vm * 60
	}

	// Validated before the audit entry, so a refused save leaves no trace of a
	// change that never happened.
	if err := s.Store.SetTemplate(name, t); err != nil {
		s.redirectTemplates(w, r, "", err.Error())
		return
	}
	if _, err := s.Store.AppendAudit(user.Username, "set-template", name,
		fmt.Sprintf("every %dm, %s profile, enabled=%v", minutes, presetName, t.Enabled)); err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	s.redirectTemplates(w, r, "Saved template "+name+". Agents pick it up on their next check-in.", "")
}

func (s *Server) postTemplateDelete(w http.ResponseWriter, r *http.Request, user auth.User) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.redirectTemplates(w, r, "", "No template named.")
		return
	}
	if err := s.Store.DeleteTemplate(name); err != nil {
		s.redirectTemplates(w, r, "", err.Error())
		return
	}
	if _, err := s.Store.AppendAudit(user.Username, "remove-template", name, ""); err != nil {
		http.Error(w, "could not record this action", http.StatusInternalServerError)
		return
	}
	msg := "Removed template " + name + "."
	if name == store.DefaultTemplateName {
		// Worth saying out loud: removing this one does not just delete a
		// template, it turns off automatic coverage for every VM that had no
		// entry of its own.
		msg += " Automatic coverage is now OFF: VMs with no entry of their own will no longer be replicated."
	}
	s.redirectTemplates(w, r, msg, "")
}
