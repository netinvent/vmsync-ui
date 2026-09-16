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

// Package api implements the agent-facing HTTP surface.
//
// The other side of this contract is cmd/vmsync-agent in the vmsync
// repository, and its client_test.go is the specification: it drives a stub
// server and pins the paths, the bearer header, the ETag exchange and the
// status codes. Anything changed here has to keep those passing.
//
//	POST /api/v1/agents/enrol            -> {"agent_id","token"}
//	POST /api/v1/agents/{id}/report      bearer; 204
//	GET  /api/v1/agents/{id}/config      bearer; long-poll, ETag-aware
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vmsync-ui/internal/store"
)

// maxPollWait bounds how long a config request may be held open, whatever
// the agent asks for. A client-supplied duration that could pin a request
// indefinitely is a denial-of-service primitive, and no legitimate agent
// needs longer than this.
const maxPollWait = 5 * time.Minute

// maxBodyBytes bounds an inventory upload. Generous for a few hundred
// domains, but finite: a malfunctioning or hostile agent must not be able
// to exhaust the UI's memory with one request.
const maxBodyBytes = 8 << 20

type Server struct {
	Store *store.Store
	Log   *slog.Logger
	// PollInterval is how often a held config request re-checks for a
	// change. Polling internally rather than waking on a write keeps this
	// simple; at one change per human action, a second of latency is
	// invisible.
	PollInterval time.Duration
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/agents/enrol", s.handleEnrol)
	mux.HandleFunc("POST /api/v1/agents/{id}/report", s.withAgent(s.handleReport))
	mux.HandleFunc("GET /api/v1/agents/{id}/config", s.withAgent(s.handleConfig))
}

type enrolRequest struct {
	Hostname       string `json:"hostname"`
	EnrolmentToken string `json:"enrolment_token"`
	AgentVersion   string `json:"agent_version"`
}

type enrolResponse struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
}

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req enrolRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Hostname == "" || req.EnrolmentToken == "" {
		http.Error(w, "hostname and enrolment_token are required", http.StatusBadRequest)
		return
	}

	agent, bearer, err := s.Store.Enrol(req.EnrolmentToken, req.Hostname, req.AgentVersion)
	if err != nil {
		// 401 for every enrolment failure, with no detail. The agent treats
		// it as terminal and says so; telling an unauthenticated caller
		// whether the token was wrong, expired, spent or meant for another
		// host would only help someone guessing.
		s.Log.Warn("enrolment refused", "hostname", req.Hostname, "remote", r.RemoteAddr)
		http.Error(w, "enrolment refused", http.StatusUnauthorized)
		return
	}
	s.Log.Info("agent enrolled", "agent_id", agent.ID, "hostname", agent.Hostname, "version", req.AgentVersion)
	writeJSON(w, http.StatusOK, enrolResponse{AgentID: agent.ID, Token: bearer})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, agent store.Agent) {
	var rep store.Report
	if err := decodeJSON(w, r, &rep); err != nil {
		return
	}
	if err := s.Store.SaveReport(agent.ID, rep); err != nil {
		s.Log.Error("could not save report", "agent_id", agent.ID, "error", err)
		http.Error(w, "could not store report", http.StatusInternalServerError)
		return
	}

	// Operation results are processed AFTER the report is stored and are
	// allowed to fail the request. That combination is deliberate: the
	// inventory is already safe on disk, and answering an error makes the
	// agent retry -- which re-sends the same results, because it keeps them
	// until this UI stops publishing the operation. So a failure here costs
	// a retry rather than losing a failover's outcome and the schedule
	// changes that follow from it.
	if err := s.Store.RecordOperationResults(agent.ID, rep.OperationResults); err != nil {
		s.Log.Error("could not record operation results", "agent_id", agent.ID, "error", err)
		http.Error(w, "could not record operation results", http.StatusInternalServerError)
		return
	}

	s.Log.Debug("report stored", "agent_id", agent.ID, "domains", len(rep.Domains), "operation_results", len(rep.OperationResults))
	w.WriteHeader(http.StatusNoContent)
}

// handleConfig answers the agent's long poll.
//
// It holds the request open until the configuration differs from the ETag
// the agent already has, or until the requested wait elapses. That is what
// delivers a change to a hypervisor within seconds while the hypervisor
// accepts no inbound connections at all.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, agent store.Agent) {
	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if wait > maxPollWait {
		wait = maxPollWait
	}
	known := r.Header.Get("If-None-Match")

	interval := s.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(wait)

	for {
		cfg, etag, err := s.Store.AgentConfigFor(agent.ID)
		if err != nil {
			s.Log.Error("could not read agent configuration", "error", err)
			http.Error(w, "could not read configuration", http.StatusInternalServerError)
			return
		}
		if etag != known {
			w.Header().Set("ETag", etag)
			// No-store: this is per-agent, changes on human action, and the
			// ETag exchange is the caching mechanism. An intermediary
			// caching it would break long-poll delivery outright.
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, cfg)
			return
		}
		if !time.Now().Before(deadline) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		select {
		case <-r.Context().Done():
			// The agent gave up or was stopped. Returning without writing
			// avoids logging a spurious error for an ordinary shutdown.
			return
		case <-time.After(minDuration(interval, time.Until(deadline))):
		}
	}
}

// withAgent authenticates the bearer token against the agent named in the
// path, and passes the resolved agent to the handler.
func (s *Server) withAgent(h func(http.ResponseWriter, *http.Request, store.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		token, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		agent, ok := s.Store.Authenticate(id, token)
		if !ok {
			// Also the answer for a revoked agent. The agent treats 401 as
			// terminal and says so loudly, which is exactly the behaviour
			// revocation wants.
			s.Log.Warn("agent request rejected", "agent_id", id, "remote", r.RemoteAddr)
			unauthorized(w)
			return
		}
		h(w, r, agent)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		// DisallowUnknownFields makes a newer agent's extra field a hard
		// error rather than silent data loss. That is the right trade for a
		// two-program contract: a version mismatch should be loud and
		// immediate, not discovered later as missing data.
		http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	if b < 0 {
		return 0
	}
	return b
}
