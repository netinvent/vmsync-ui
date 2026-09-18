# vmsync-ui

The control-plane console for [vmsync](https://github.com/netinvent/vmsync)
replication. Agents on each hypervisor enrol here and report what their host
knows about its own replication state; operators read the availability page.

Its own module, with **no dependencies outside the Go standard library**. It
never touches libvirt, libnbd or any of vmsync's Go packages — it only speaks
JSON over HTTPS to agents — so it builds with a plain `go build` and none of
the native headers vmsync itself needs.

## What it does, and what it deliberately cannot

This is **phase 4 of the control plane**: agents report, the console sets
the replication schedule they run, and failovers are driven from it.

What it still cannot do is act on a VM directly. It has no session to any
hypervisor and issues no command; it publishes a schedule, and each agent
decides what to run on its own host. So the console being unreachable does
not stop replication — agents keep running the last schedule they cached.

A failover is issued the same way: as a one-shot **operation** the agent
collects on its next poll and carries out itself. The console never opens a
session to a hypervisor; it publishes an instruction, and the agent that owns
the host decides whether to honour it.

The one thing to understand about the schedule is that it names *when*, not
*whether*. Removing an entry stops the timer; it does not stop anything else
invoking vmsync for that VM. To make a VM genuinely refuse to be
overwritten, set its replication role (`vmsync -update-role=paused`), which
vmsync enforces itself and which does not depend on this console at all.

It holds no hypervisor credentials and reaches into no host. Agents dial
**out** to it, so hypervisors need no inbound port — which is what makes a DR
site behind its own firewall workable.

It stores no inventory of replication pairs. The topology lives in each VM's
own libvirt metadata and is rediscovered by agents on every report, so this
console cannot drift out of step with reality about what replicates where.

## Build and install

```bash
go build -o vmsync-ui ./cmd/vmsync-ui
install -m 0755 vmsync-ui /usr/local/bin/
mkdir -p /etc/vmsync-ui
cp vmsync-ui.conf.example /etc/vmsync-ui/vmsync-ui.conf
```

## Configure

Generate a password hash per account — plaintext passwords are refused at
startup, with a message naming this command:

```bash
vmsync-ui -hash-password
```

Then edit `/etc/vmsync-ui/vmsync-ui.conf`:

| key | meaning |
| --- | --- |
| `listen` | Address to serve on. Both the agent API and the console share it. |
| `tls_cert` / `tls_key` | Required. Agents refuse a non-`https://` UI. |
| `state_dir` | Agents, their token hashes, reports and the audit log. |
| `grafana_url` | Optional. Adds a "Trends" link; this console deliberately draws no time series. |
| `session_hours` | How long a sign-in lasts. Default 12. |
| `accounts` | `username`, `password_hash`, `role` (`admin` or `readonly`). |
| `insecure` | Serves plain HTTP for local development. Agents will refuse to talk to it, so this is browser-only. |

An unrecognised key is a startup error rather than a setting that silently
does nothing — in a file carrying TLS paths and account roles, a quietly
ignored typo is the worst outcome.

### Roles

| capability | readonly | admin |
| --- | --- | --- |
| View availability, agents, audit, failover state | yes | yes |
| Enrol and revoke agents | no | yes |
| Set the schedule | no | yes |
| Promote, invert, shut down, set a role, cancel | no | yes |

A reader sees a split brain and every failover the estate has performed;
what they do not get is anything to click. Withholding the state would be the
wrong half to withhold — the people watching the board are often exactly the
ones who need to raise the alarm.

The split that matters most: `-verify=compare` and
`-verify=fast` **suspend the source VM**, and only `-verify=online` does not.
Read-only accounts get `online` alone — "just a verification" must not be a
production-impacting action for someone who was given the lesser role.

Attribution only means something if accounts are not shared. "Who failed over
production at 3am" is a question that gets asked eventually.

## Enrol an agent

On the **Agents** page, create a token for a hostname. It is shown once,
is single-use, is bound to that hostname, and expires in 24 hours. Then on
the hypervisor, follow agent enrolling procedure.

That exchanges the token for a long-lived credential and sends one report.
Start the service afterwards.

### TLS and agents

If the UI has a **publicly-trusted certificate, agents need nothing** — no CA
file, and renewals are invisible to them. The agent's `--ui-ca` flag is only
for a private CA (distribute the CA once; it outlives many leaf renewals) or
a bare self-signed certificate (which does mean touching every host on every
renewal — prefer a small private CA).

## Reading the availability page

Ordered worst-first, because the page is read to find what needs attention.

- **Not replicated** gets its own alarm-styled panel. A VM nobody configured
  replication for is otherwise *absent* from a green board and looks fine.
- **Hosts with no agent** are listed as blind spots. "I cannot see this" must
  never render as "this is healthy".
- **Target missing** gets its own alarm-styled panel. A source whose named
  replica is in no agent's report under that exact name — deleted, never
  created, misspelled, or a short name where the agent reports an FQDN —
  has nowhere for its syncs to land. Each row is shown verbatim, with a
  note saying whether any agent reports under that name at all. A
  reference is skipped when the source already has a pair row for a target
  under the same VM name: the pair proves the copy exists, so what remains
  is a spelling difference, not a missing copy.
- References match **exactly** (case-insensitive only): `hypervisor01` in
  hand-typed vmsync metadata does not resolve to an agent reporting as
  `hypervisor01.domain.tld`. That surfaces as rows to fix — align the names
  and the rows clear — rather than being silently correlated away.
- **Behind** is the replication lag — how much you would lose. A never-synced
  target shows `—` rather than `0`, which would read as "just synced".
- **promoted** and **paused** get a distinct neutral colour. They are
  deliberate states; if a planned failover turned the board red, people would
  learn to ignore red.
- Reasons appear on the row, not behind a click.

Freshness always comes from the **target**, because that is where vmsync
writes `last_sync`, `last_checkpoint` and `failure_count`. A source's own
metadata records where it replicates to, never when.

## The agent-facing API

```
POST /api/v1/agents/enrol            -> {"agent_id","token"}
POST /api/v1/agents/{id}/report      bearer; 204
GET  /api/v1/agents/{id}/config      bearer; long-poll, ETag-aware
```

`internal/api/agents_test.go` here and `client_test.go` in the agent are the
two halves of this contract, each driving a stub of the other. Editing one
without the other is how agents in the field stop working.

Notes for anyone changing it:

- **401 means revoked** and the agent treats it as terminal, saying so
  loudly. Every enrolment failure returns a bare 401 with no detail — an
  unauthenticated caller learns only that it did not work.
- **The config endpoint must genuinely hold the request** for the `wait` it
  is given before answering 304. That hold is what delivers a change to a
  hypervisor in seconds without any inbound connection.
- Bearer tokens are stored as SHA-256 hashes. If `agents.json` leaks, what
  leaks is hashes.

## Security notes

- No `WriteTimeout` on the HTTP server, deliberately: agents long-poll for
  minutes, and a write deadline would sever those mid-hold. `ReadHeaderTimeout`
  and `IdleTimeout` cover slow-client abuse instead.
- Sessions are in memory only. A restart signs everyone out and there is no
  session file to leak.
- Forms carry a per-session CSRF token; the session cookie is
  `HttpOnly`, `Secure` and `SameSite=Strict`.
- Passwords are PBKDF2-HMAC-SHA256 at 600k iterations, from the standard
  library as of Go 1.24.

## Split-brain detection

The availability page raises one alarm above everything else: a pair where the
target has been **promoted and is running while its original source is running
too**. Two live copies of one VM, diverging from the moment the second started.

A promotion issued from the failover page can **arm a fence** against the
displaced source, and that closes the common case: the old source's agent
reads the token from the promoted domain's own libvirt and shuts its copy
down cleanly. Detection still matters, because fencing is cooperative and
therefore has three ways not to happen — the displaced host may have no
agent, its agent may be unable to reach the promoted peer to read the token,
or the guest may ignore the shutdown, which is latched rather than retried.
None of those is rare during exactly the partition that motivated the
failover, and a promotion performed without arming a fence at all (a drill,
or from a shell) authorises nothing.

So this console does not claim to prevent split brain; it has no power
control and never destroys a running guest. What it can always do is
*notice*: neither agent can see the other, so the only place the condition is
visible at all is here, where both reports arrive.

It is deliberately narrow — promoted **and** target running **and** source
running **and** the source's report *current*. That last condition is what keeps
the alarm worth reading. Reports are stored in place and never aged out, so a
host that died with its VM running keeps saying `Active: true` forever; without
a freshness gate the banner would fire on every correctly-executed failover of a
dead primary, and an alarm indistinguishable from success is one operators learn
to scroll past.

A promoted target whose source has simply gone quiet is reported separately and
without alarm, showing how long ago the source was last heard from. That is the
honest state: silence is not proof the old primary is down, and it is also not
evidence that it is up. It becomes the real alarm only if that host starts
reporting again with the VM still running.

## The schedule page

Only **source** VMs are listed. A target is synced by the agent on its
*source* host, so an entry against a target's own agent would be one that
agent could never act on — the page does not offer it rather than accept a
schedule that silently does nothing.

Each entry is an interval, a profile and an optional verification mode:

| Profile  | For                        | Settings                              |
|----------|----------------------------|---------------------------------------|
| `wan`    | a link you pay for         | zstd-5, network buffer, I/O depth 16  |
| `lan`    | a switched site network    | zstd-1, I/O depth 8                   |
| `direct` | same host or a fast fabric | no compression, I/O depth 8           |

The profile is resolved to explicit vmsync flags **here**, and travels to
the agent as those fields — never as the name `wan`. The agent therefore
needs no opinion about what `wan` means, and what an operator reads on the
page is what will run.

`-verify=compare` and `-verify=fast` **suspend the source VM** for the
comparison. `-verify=online` does not. The page says so on the control, and
the server checks it again on save rather than trusting the form.

A saved change reaches a host on that agent's next poll, normally within
seconds. Two estate-wide limits bound what that can cost: how many syncs one
agent runs at once, editable under **Estate defaults** below, and how many
may target the same host, which is stored and served but has no form yet.

## Operations

Promotions, inversions, clean shutdowns and role changes are published as
**operations** — one-shot instructions for one agent, alongside the standing
schedule. They ride in the same document an agent long-polls, so issuing one
*is* publishing it, and acknowledging one *is* removing it.

The lifecycle, and what each step protects against:

1. An admin confirms. The UI writes an audit entry recording intent, then
   the operation, with a **15-minute deadline**.
2. It appears in that agent's config, moving the ETag, so a polling agent
   sees it within seconds.
3. The agent executes it **exactly once ever**, against a durable ledger
   that records intent before acting.
4. The result rides on every report the agent sends until this UI stops
   publishing the operation.
5. The UI applies the consequences, records the result, and stops publishing
   — which is what tells the agent to forget it.

**Only one operation per VM may be in flight.** Two promotions of one
domain, or a promote racing an invert, is not a state anyone should be able
to create by clicking twice on a slow page.

**Consequences are applied before the result is recorded.** A crash between
them leaves the operation still published and the agent still re-reporting
it, so the whole step simply runs again — it is idempotent and keyed by
operation ID. The other order would acknowledge the operation and lose the
schedule changes forever.

Those consequences are the part whose absence silently breaks a pair:

- A successful **promotion** disables the old source's schedule entry.
  Otherwise it keeps firing every interval against a domain that is now
  live, is refused by vmsync each time, and climbs `failure_count`.
  Disabled, not deleted, so the profile survives for the inversion.
- A successful **inversion** moves the schedule entry to the new source's
  agent in a *single* write, flipping its target host. Two writes would let
  a crash land the entry under neither agent, and a missing schedule entry
  is indistinguishable from a VM nobody asked to replicate — the pair would
  stop silently, right after a failover.

The migrated entry also gets its **`target_disk_path` re-aimed**. That value
says where *this direction's* replicas go, so after an inversion it names the
new source's own disks — carried across unchanged, the reversed sync would
write to the wrong directory on the wrong host, and where that directory
happens to exist it would redefine the domain to match and orphan the
original disk. It is recomputed from where the new target's disks actually
are, read out of that host's own report, and left alone when they span more
than one directory (a single value cannot express that in either direction).

The migrated entry arrives **disabled**. The first sync in the reversed
direction has no checkpoint chain and must be a full reinit, which the
schedule cannot yet express; enabled, it would schedule a run that fails
every interval.

An operation can be **cancelled** while it has not reported. After that it
fails loudly rather than pretending — the work has happened on a hypervisor
and no UI state undoes it. An expired operation is still published on
purpose: the agent must see it, refuse it and report the refusal, or the
audit entry hangs open forever with nothing saying what became of it.

### Upgrade order

**UI first, then agents.** The report body is decoded with
`DisallowUnknownFields`, so an agent that sends a field this UI does not know
has its *entire* report rejected — domains, roles and sync results included —
and the symptom looks like every upgraded host going offline at once.

This has applied to `operation_results`, and applies again to the fence
fields (`fence_id`, `fence_source`, `fence_armed_at_unix`, `fence_armed_by`
and the `fenced` object). It applies to every future addition too, which is
why both halves of the contract are pinned by tests that name the strings
literally: `TestAReportCarryingFenceStateIsAccepted` here, and
`TestSendReportCarriesFenceStateUnderTheAgreedNames` in the agent. Changing
one without the other fails there rather than in the field.

**The other direction is lenient, deliberately.** An agent decodes the
config it polls without `DisallowUnknownFields`, so a newer UI sending a
field an older agent has never heard of — `shutdown_timeout_sec`, say — is
ignored by that agent rather than breaking it. That asymmetry is what makes
"UI first" a safe order rather than merely a preferred one: a UI ahead of
its agents degrades to the old behaviour, while agents ahead of their UI
stop reporting entirely.

(A `--standalone` file *is* parsed strictly, for the opposite reason: it was
typed by a person, and a silently ignored key there looks like the scheduler
not working.)

## The failover page

`/failover` is where a failover is actually driven. It lists every domain
that participates in replication, and beside each one **only the actions its
current state allows**.

That restriction is the page's whole safety design. A console that offers
every action on every row is one that invites the wrong one during an
incident, when the person reading it is under pressure and moving fast:

| state | what is offered | why not the others |
| --- | --- | --- |
| an ordinary replica | **Promote** | there is nothing to invert until a failover has happened |
| a source | **Shut down**, and **Invert** once its target is promoted | promoting a source would ask vmsync to overwrite the original with its own copy |
| promoted and running | **Shut down**, **Set role** | re-promoting does nothing; invert makes it permanent |
| promoted, never started | **Finish the failover** | vmsync writes the promotion record *before* booting, so this is what a crash or a refused start leaves behind — and it must be finishable from here |
| paused, including anything a fence stopped | **Set role** | the way back |

None of it replaces the checks underneath. vmsync refuses a promotion whose
replica is not usable, and the agent refuses an operation whose peer does not
match the VM's own libvirt metadata. This layer exists so the common case
never reaches those refusals.

**Rows that need a decision sort first** — a split brain above all, then
promoted domains, then the old sources of unresolved failovers, then
anything paused. Nobody should have to scroll to find the VM that is running
twice.

### What only this page can know

An agent cannot see another agent. A source's own metadata records where it
replicates *to*, never that the target has since been promoted — so the
Invert action, and the split-brain banner, exist only because the control
plane hears from both hosts and cross-references them.

### Storage, beside the decision that spends it

Each row shows what the domain's disks occupy and what is free on the
storage under them, because an inversion's `-replaced-disk-action=rename`
keeps the displaced copy and therefore needs room for both at once. Where
the figures say it will not fit, the row says so. Where nothing measured
them, it shows a dash rather than `0 B` — a dash is missing data, and `0 B`
reads as a measurement.

### Arming a fence

The promote form has a **stop the old source** checkbox. Ticking it arms a
fence: the displaced source's agent reads the token from the promoted
domain's own libvirt and shuts that VM down, so one VM does not end up
serving in two places. It is off by default and opt-in the whole way down —
a DR drill is a promotion too, and a drill that stopped production would be
worse than the split brain it was rehearsing for. See the fencing section of
the [main README](../vmsync/README.md).

Note what does *not* travel: which source to fence. The agent has vmsync
resolve that from the promoted domain's own `replica_source`, so this
console can request a fence but can never choose its victim.

### Setting a role by hand

`target`, `source` and `paused` only. **`promoted` is deliberately absent**:
promotion has real preconditions — vmsync verifies a usable replica exists
and reports the data-loss window it accepts — and writing the role directly
would reach the same recorded state having checked none of them. The button
for that is three columns to the left.

This is also the way back from a fence: a fenced VM is `paused`, and setting
it to `target` lets it receive again.

### Cancelling

Any operation that has not reported can be cancelled, which stops it being
published and frees that VM for another. It does not undo one an agent has
already run — the work happened on a hypervisor, and no console state
changes that.

### Fenced, or merely paused

A VM a fence stopped and a VM an operator paused are **both just `paused`**
in libvirt. Nothing in the metadata tells them apart, and they call for
completely different responses. So agents report what their own fence ledger
says, and the row shows it:

- **`fenced`** — stopped by vmsync because a named peer was promoted, with
  when, by whom, and which copy displaced it. Explicitly *not* an
  administrative pause.
- **`fence failed`** — a fence was attempted and the domain is **still
  running**. This one exists nowhere else: the attempt leaves no mark in
  libvirt, the fence is latched so nothing retries it, and the VM simply
  keeps running beside a promoted copy — indistinguishable, without this,
  from a failover nobody has got to yet. It sorts above everything, including
  the inferred split brain, because something already tried to resolve this
  and could not.

The alarm clears when the domain stops, not when the ledger changes: the
ledger entry is latched forever by design, so a fence that failed in March
and was then handled by hand would otherwise still be shouting in December.

A promoted row also shows whether **its own** promotion armed a fence, and
against whom. Absence is the drill.

### How long a guest gets to stop

A clean shutdown that overruns is reported as a **failure** — and when a
fence asked for it, fences latch, so nothing tries again and the console
shows a live split brain that is really just a database taking its time.
Setting this per VM is what avoids that.

Three steps, in order: the VM's own value on its schedule row, the estate
default under **Estate defaults** on the schedule page, then vmsync's own
300 seconds. A blank per-VM box inherits, and shows the estate value as its
placeholder.

The **same order is implemented in the agent**, because the two paths that
shut a domain down are not the same code:

- a **shutdown operation** carries the value resolved *when it was issued*,
  so the instruction means the same thing whenever it runs — one that
  silently meant 300 seconds in March and 900 in April, because somebody
  edited a setting in between, is not one anybody can audit;
- a **fence** has no operation behind it and resolves from the config it
  last polled, because during the partition that usually causes one there is
  no control plane to ask.

The agent **clamps** whatever it is sent (30s–3600s) rather than trusting
it: this UI is a separately-versioned program, and the number decides how
long a production VM is given before its shutdown is called a failure.

## Not built yet

On-demand "sync now". The per-host target budget is stored and served but
has no form (it is a row per host, unlike the two defaults above). `reinit`
exists as an operation kind and is not implemented on the agent side.
