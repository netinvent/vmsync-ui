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

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"vmsync-ui/internal/store"
)

// These mirror, from the server side, what cmd/vmsync-agent's own
// client_test.go asserts from the client side. The two files together are
// the contract between the programs: if one is edited without the other,
// agents in the field stop working.

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	srv := &Server{
		Store:        st,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 10 * time.Millisecond,
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

// enrolAgent mints a token and redeems it the way an agent would, returning
// the agent id and bearer token.
func enrolAgent(t *testing.T, srv *Server, ts *httptest.Server, hostname string) (string, string) {
	t.Helper()
	clear, err := srv.Store.CreateEnrolmentToken(hostname, "tester", time.Hour)
	if err != nil {
		t.Fatalf("create enrolment token: %v", err)
	}
	body, _ := json.Marshal(enrolRequest{Hostname: hostname, EnrolmentToken: clear, AgentVersion: "0.30"})
	resp, err := http.Post(ts.URL+"/api/v1/agents/enrol", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enrol returned %s, want 200", resp.Status)
	}
	var out enrolResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode enrol response: %v", err)
	}
	if out.AgentID == "" || out.Token == "" {
		t.Fatal("enrol response is missing agent_id or token -- the agent refuses this outright")
	}
	return out.AgentID, out.Token
}

func TestEnrolmentTokenIsSingleUse(t *testing.T) {
	srv, ts := newTestServer(t)
	clear, err := srv.Store.CreateEnrolmentToken("hyper01p", "tester", time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	body, _ := json.Marshal(enrolRequest{Hostname: "hyper01p", EnrolmentToken: clear, AgentVersion: "0.30"})

	first, err := http.Post(ts.URL+"/api/v1/agents/enrol", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first enrol: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first enrol = %s, want 200", first.Status)
	}

	// Replaying the same token must fail. This is what makes it safe to
	// move an enrolment token around by whatever means an operator has.
	second, err := http.Post(ts.URL+"/api/v1/agents/enrol", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second enrol: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed token = %s, want 401 -- the agent maps 401 to a terminal error", second.Status)
	}
}

func TestEnrolmentTokenIsBoundToItsHostname(t *testing.T) {
	// A token is minted for one named host. Honouring it elsewhere would
	// turn a leaked token into an enrolment anywhere in the estate.
	srv, ts := newTestServer(t)
	clear, _ := srv.Store.CreateEnrolmentToken("hyper01p", "tester", time.Hour)
	body, _ := json.Marshal(enrolRequest{Hostname: "someone-elses-box", EnrolmentToken: clear})

	resp, err := http.Post(ts.URL+"/api/v1/agents/enrol", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("enrol with a mismatched hostname = %s, want 401", resp.Status)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	srv, ts := newTestServer(t)
	clear, _ := srv.Store.CreateEnrolmentToken("hyper01p", "tester", -time.Minute)
	body, _ := json.Marshal(enrolRequest{Hostname: "hyper01p", EnrolmentToken: clear})

	resp, err := http.Post(ts.URL+"/api/v1/agents/enrol", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired token = %s, want 401", resp.Status)
	}
}

func TestReportRequiresTheAgentsOwnBearerToken(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")
	otherID, _ := enrolAgent(t, srv, ts, "hyper02p")

	body, _ := json.Marshal(store.Report{Hostname: "hyper01p"})
	cases := []struct {
		name, id, token string
		want            int
	}{
		{"its own token", id, token, http.StatusNoContent},
		{"no token at all", id, "", http.StatusUnauthorized},
		{"a wrong token", id, "not-the-token", http.StatusUnauthorized},
		{"another agent's id with this token", otherID, token, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+tc.id+"/report", bytes.NewReader(body))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("report: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("report = %s, want %d", resp.Status, tc.want)
			}
		})
	}
}

func TestRevokedAgentIsRejected(t *testing.T) {
	// Revocation has to work from day one: a decommissioned or compromised
	// host must be cuttable without waiting for a credential to expire.
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")
	if err := srv.Store.Revoke(id); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	body, _ := json.Marshal(store.Report{Hostname: "hyper01p"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+id+"/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked agent = %s, want 401 -- the agent maps this to a terminal, loudly-logged error", resp.Status)
	}
}

func TestReportIsStoredAndReadableBack(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	sent := store.Report{
		ReportedAtUnix: 1_800_000_000,
		AgentVersion:   "0.30",
		Hostname:       "hyper01p",
		LibvirtURI:     "qemu:///system",
		Domains: []store.ReportDomain{{
			Name:          "web01",
			ReplicaSource: "hyper01p:web01",
			Status:        "ok",
			AgeSeconds:    42,
		}},
	}
	body, _ := json.Marshal(sent)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+id+"/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report = %s, want 204", resp.Status)
	}

	got, ok, err := srv.Store.Report(id)
	if err != nil || !ok {
		t.Fatalf("Report() = ok:%v err:%v, want the stored report", ok, err)
	}
	if len(got.Domains) != 1 || got.Domains[0].Name != "web01" || got.Domains[0].Status != "ok" {
		t.Errorf("stored report = %+v, want the domain as sent", got.Domains)
	}

	// Reporting also stamps the agent as seen, which is what lets the UI
	// distinguish a quiet agent from an absent one.
	agents, err := srv.Store.Agents()
	if err != nil {
		t.Fatalf("Agents(): %v", err)
	}
	if len(agents) != 1 || agents[0].LastSeenAt == 0 {
		t.Errorf("agent record = %+v, want last_seen_at set by the report", agents)
	}
}

// The report body is decoded with DisallowUnknownFields, so every field an
// agent sends must exist here or its ENTIRE report is rejected -- domains,
// roles, sync results and all. That makes this the test that catches a
// half-applied protocol change, which otherwise looks in production like
// every upgraded host going offline at once.
//
// Raw JSON rather than a marshalled struct on purpose: marshalling from the
// same type this decodes into would agree with itself no matter what the
// agent actually sends. These field names are copied from the agent's own
// wire tags.
func TestAReportCarryingFenceStateIsAccepted(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	const body = `{
	  "reported_at_unix": 1800000000,
	  "agent_version": "0.40",
	  "hostname": "hyper01p",
	  "libvirt_uri": "qemu:///system",
	  "domains": [
	    {
	      "name": "web01",
	      "active": true,
	      "role": "source",
	      "failure_count": 0,
	      "replica_targets": ["hyper02p:web01"],
	      "fenced": {
	        "fence_id": "f1",
	        "state": "failed",
	        "at_unix": 1799999000,
	        "peer_ref": "hyper02p:web01",
	        "armed_by": "alice",
	        "error": "the guest did not shut down in time"
	      },
	      "status": "ok",
	      "age_seconds": 42
	    },
	    {
	      "name": "db01",
	      "active": true,
	      "role": "promoted",
	      "failure_count": 0,
	      "replica_source": "hyper00p:db01",
	      "fence_id": "f2",
	      "fence_source": "hyper00p:db01",
	      "fence_armed_at_unix": 1799998000,
	      "fence_armed_by": "bob",
	      "status": "ok",
	      "age_seconds": 7
	    }
	  ]
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+id+"/report", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report = %s, want 204 -- a field the agent sends is missing here, "+
			"which rejects the whole report and not just that field", resp.Status)
	}

	got, ok, err := srv.Store.Report(id)
	if err != nil || !ok {
		t.Fatalf("Report() = ok:%v err:%v", ok, err)
	}
	if len(got.Domains) != 2 {
		t.Fatalf("stored %d domains, want 2", len(got.Domains))
	}

	fenced := got.Domains[0].Fenced
	if fenced == nil {
		t.Fatal("the fence ledger entry did not survive decoding")
	}
	if fenced.State != "failed" || fenced.PeerRef != "hyper02p:web01" || fenced.ArmedBy != "alice" {
		t.Errorf("fenced = %+v, want the entry as sent", fenced)
	}
	if fenced.Error == "" {
		t.Error("the agent's reason for a failed fence must survive; it is the whole diagnostic")
	}
	if fenced.Succeeded() {
		t.Error("a failed fence must not report as succeeded")
	}

	armed := got.Domains[1]
	if armed.FenceSource != "hyper00p:db01" || armed.FenceArmedBy != "bob" || armed.FenceArmedAtUnix == 0 {
		t.Errorf("the armed fence token did not survive: %+v", armed)
	}
}

func TestConfigReturnsAnETagAndThen304(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	get := func(etag string, wait int) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/"+id+"/config?wait="+strconv.Itoa(wait), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		return resp
	}

	first := get("", 0)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first config = %s, want 200", first.Status)
	}
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first config response -- the agent needs it to ask for 304 next time")
	}
	var cfg store.AgentConfig
	json.NewDecoder(first.Body).Decode(&cfg)
	if cfg.ReportIntervalSeconds <= 0 || cfg.PollWaitSeconds <= 0 {
		t.Errorf("config = %+v, want usable defaults -- the agent normalizes these, but it should not have to", cfg)
	}

	second := get(etag, 0)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("config with a matching ETag = %s, want 304", second.Status)
	}
}

func TestConfigLongPollHoldsThenReturns304(t *testing.T) {
	// The hold is what delivers a change to a hypervisor in seconds without
	// the hypervisor accepting any inbound connection. If the server
	// answered 304 immediately, agents would busy-poll instead.
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	_, etag, err := srv.Store.AgentConfigFor(id)
	if err != nil {
		t.Fatalf("AgentConfigFor(): %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/"+id+"/config?wait=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-None-Match", etag)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("config = %s, want 304 after the hold elapsed", resp.Status)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("returned after %s, want it held for about the requested 1s", elapsed)
	}
}

func TestConfigLongPollReturnsEarlyWhenTheConfigChanges(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	_, etag, _ := srv.Store.AgentConfigFor(id)

	// Change the configuration while a poll is in flight; the held request
	// must return promptly rather than sitting out its full wait.
	go func() {
		time.Sleep(100 * time.Millisecond)
		srv.Store.SetSettings(store.Settings{
			ReportIntervalSeconds: 120,
			PollWaitSeconds:       45,
			MaxConcurrentSyncs:    4,
		})
	}()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/"+id+"/config?wait=10", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-None-Match", etag)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config = %s, want 200 with the new configuration", resp.Status)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s to notice the change, want it delivered promptly", elapsed)
	}
	var cfg store.AgentConfig
	json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg.ReportIntervalSeconds != 120 || cfg.MaxConcurrentSyncs != 4 {
		t.Errorf("delivered %+v, want the configuration just set", cfg)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	// A malfunctioning or hostile agent must not be able to exhaust the
	// UI's memory with one request.
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper01p")

	huge := bytes.Repeat([]byte("a"), maxBodyBytes+1024)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+id+"/report", bytes.NewReader(huge))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Error("an oversized body was accepted")
	}
}

// The same upgrade-order hazard as the fence test above, for restore points.
//
// Raw JSON, and the field names copied from the agent's own wire tags rather
// than marshalled from this package's type: a round trip through the same
// struct would agree with itself no matter what the agent actually sends.
func TestAReportCarryingRestorePointsIsAccepted(t *testing.T) {
	srv, ts := newTestServer(t)
	id, token := enrolAgent(t, srv, ts, "hyper02p")

	const body = `{
	  "reported_at_unix": 1800000000,
	  "agent_version": "0.41",
	  "hostname": "hyper02p",
	  "libvirt_uri": "qemu:///system",
	  "domains": [
	    {
	      "name": "web01",
	      "active": false,
	      "role": "target",
	      "failure_count": 0,
	      "replica_source": "hyper01p:web01",
	      "restored_from": "1756030000-vmsync-cpt-000041",
	      "restored_at_unix": 1756900000,
	      "restored_by": "alice",
	      "restore_points": [
	        {
	          "tag": "1756041600-vmsync-cpt-000042",
	          "taken_at_unix": 1756041600,
	          "checkpoint_at_unix": 1756041000,
	          "checkpoint": "vmsync-cpt-000042",
	          "source": "hyper01p:web01",
	          "verify": "passed",
	          "disks": ["web01.qcow2"]
	        },
	        {
	          "tag": "1756030000-vmsync-cpt-000041",
	          "taken_at_unix": 1756030000,
	          "incomplete": true
	        }
	      ],
	      "status": "ok",
	      "age_seconds": 12
	    }
	  ]
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+id+"/report", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report = %s, want 204 -- a field the agent sends is missing here, "+
			"which rejects the whole report and not just that field", resp.Status)
	}

	got, ok, err := srv.Store.Report(id)
	if err != nil || !ok {
		t.Fatalf("Report() = ok:%v err:%v", ok, err)
	}
	rps := got.Domains[0].RestorePoints
	if len(rps) != 2 {
		t.Fatalf("stored %d restore points, want 2", len(rps))
	}
	if rps[0].Tag != "1756041600-vmsync-cpt-000042" || rps[0].Verify != "passed" {
		t.Errorf("restore point = %+v, want the entry as sent", rps[0])
	}
	// checkpoint_at is what the page measures a rollback's reach from, and it
	// is NOT taken_at: a checkpoint precedes the copy it names, so losing this
	// would make every copy look fresher than it is.
	if rps[0].CheckpointAtUnix != 1756041000 {
		t.Errorf("checkpoint_at = %d, want 1756041000", rps[0].CheckpointAtUnix)
	}
	if len(rps[0].Disks) != 1 {
		t.Errorf("the disk list did not survive: %+v", rps[0].Disks)
	}
	// One whose sidecar could not be read is still listed. The directory is
	// the inventory, and hiding an entry that exists on disk would make this
	// disagree with what -list-restore-points shows on the host.
	if !rps[1].Incomplete {
		t.Error("an incomplete restore point must survive as incomplete, not be dropped")
	}

	// The restore record travels on the same domain object. It is the one
	// piece of this that SURVIVES a promotion -- afterwards role=paused has
	// been overwritten and every other trace of a rollback is ambiguous with
	// an ordinary lagging replica -- so losing it on the wire would mean the
	// UI could never explain why a promoted copy is as old as it is.
	d := got.Domains[0]
	if d.RestoredFrom != "1756030000-vmsync-cpt-000041" {
		t.Errorf("restored_from = %q, want the tag as sent", d.RestoredFrom)
	}
	if d.RestoredAtUnix != 1756900000 {
		t.Errorf("restored_at_unix = %d, want 1756900000", d.RestoredAtUnix)
	}
	if d.RestoredBy != "alice" {
		t.Errorf("restored_by = %q, want alice -- a control plane's audit log does not survive losing the control plane, and this does", d.RestoredBy)
	}
	// restored_at is the ROLLBACK's instant, not the copy's: the sent values
	// differ deliberately, and conflating them would defeat the timeline this
	// exists to establish.
	if d.RestoredAtUnix == rps[1].TakenAtUnix {
		t.Error("restored_at_unix equals the restore point's taken_at; they are different instants")
	}
}
