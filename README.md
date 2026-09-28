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

The same split is why a warning here is only ever a warning. When the failover
page says a replica is known not to match its source, it is repeating a verdict
vmsync wrote and vmsync enforces — the engine refuses that promotion whatever
this page shows. So the control stays offered rather than being withdrawn:
taking it away would hide the explanation without removing the ability, since
the same command remains available on the hypervisor itself.

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
- **Verify failed** is a second pill *beside* the status, never instead of it,
  with the date the finding was recorded. The status answers how far **behind**
  a copy is and goes on saying `ok` for one that was compared against its
  source and did not match, so folding the two together would make the row
  choose between two facts that are both true — and dropping the marker would
  let a copy known to be wrong read as healthy. The target cell spells out what
  it means: vmsync refuses every sync into the replica while the finding
  stands, a plain full resync included, and the repair is a sync set to recopy
  once and re-verify, which clears it only if that second comparison passes.
- **Partial copy** is a third pill, also *beside* the status, for a replica
  whose rebuild was interrupted. It is the one marker on the board that
  contradicts every other cell on its row, and the reason is worth
  understanding: a full copy into an existing target renames the good disks to
  `<disk>.vmsync-replaced-<stamp>` and writes new base images in their place
  *without* touching the domain's metadata, which is only updated when the run
  finishes. So a run that dies part way leaves a recent checkpoint, a small
  lag and a failure count of zero — all truthful, and all about the copy that
  was renamed aside rather than the disks now sitting there. Nothing else on
  the board moves. The dated explanation on the target cell says the replica is
  a partial copy, that the metadata beside it describes the copy the rebuild
  replaced, and that the complete copy may still be on the host under the
  `.vmsync-replaced-<stamp>` suffix; the source cell repeats the headline,
  because a resync runs from the source and the source's own metadata knows
  none of this.
- Reasons appear on the row, not behind a click.

Freshness always comes from the **target**, because that is where vmsync
writes `last_sync`, `last_checkpoint` and `failure_count`. A source's own
metadata records where it replicates to, never when. The verification record
(`verify_state`, `verify_failed_at`) is read from the target and the same
report, but it is not a freshness fact at all: freshness says how far behind a
copy is, the record says it does not match, and a copy that is wrong does not
become right by being recent.

`replica_incomplete` comes from the target too, and it is the one field that
tells you the freshness figures beside it are about **different disks**. It is
carried raw and parsed only where it is rendered; nothing in this console
branches on it. The refusal it exists to justify lives in vmsync, on the host
holding the disks, because `-promote` runs there during a disaster with the
source host gone — so a value this console cannot parse still renders as a
warning, with the agent's own text shown verbatim, rather than disappearing.

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

### The correlation id

Every operation carries an **`action_id`**, and it is this UI's own audit
entry id rather than a second identifier minted for the purpose. The agent
passes it to the engine as `-action-id`, and the engine stamps it into the
intent and outcome records it journals beside the disks it touched.

That is what closes a loop across three programs that keep separate records
and share no storage: the audit entry says who asked and why, the operation
says what was published to which agent, and the journal on the hypervisor says
what the engine then did. The case it exists for is the one nobody plans for
— somebody is holding a replica whose rebuild died, and needs to know what
ran, under whose hand, and whether it was the first attempt or the third.
Correlating three records by timestamp answers that until a run was retried,
which is exactly the situation an interrupted rebuild tends to produce.

It is optional and `omitempty` in both directions. An operation issued before
the field existed carries none, an operation issued with no audit entry
carries none rather than a fabricated id pointing at an intent nobody wrote,
and an agent too old to know the field ignores it — the config an agent polls
is decoded leniently, unlike the reports it sends.

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

This has applied to `operation_results`, to the fence fields (`fence_id`,
`fence_source`, `fence_armed_at_unix`, `fence_armed_by` and the `fenced`
object), to the verification record (`verify_state`, `verify_failed_at_unix`),
and now to `replica_incomplete`. It applies to every future addition too,
which is why both halves of the contract are pinned by tests that name the
strings literally: `TestAReportCarryingFenceStateIsAccepted`,
`TestAReportCarryingAVerificationFailureIsAccepted` and
`TestAReportCarryingAnInterruptedRebuildIsAccepted` here, and
`TestSendReportCarriesFenceStateUnderTheAgreedNames` and
`TestSendReportCarriesVerifyStateUnderTheAgreedNames` in the agent. Changing
one without the other fails there rather than in the field.

The verification fields and `replica_incomplete` are all `omitempty`, which is
why the order still matters in spite of how rare each condition is: a report
from an agent ahead of its UI decodes cleanly for every healthy domain and is
rejected outright the first time a replica fails a verification or a rebuild
is interrupted — the symptom would arrive weeks after the upgrade that caused
it, on exactly the host with something wrong.

For `replica_incomplete` the order is **UI first, then engines, then agents**,
because a third program is involved: the engine is what arms and clears the
field, and `-promote` is what refuses on the strength of it. That refusal
rests on this field alone — nothing writes a `failure_count` alongside it as a
second carrier — so an engine too old to know the field will still accept a
half-written replica, on the DR host, during the incident. **Every host that
drives syncs or promotions has to be upgraded before the refusal can be relied
on.** Until then this console's warnings are the only thing standing between
an operator and a partial copy, which is an argument for reading them, not for
trusting them as an interlock.

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

### A replica known not to match its source

A `-verify` that found a replica's contents differing from its source leaves
the verdict on the domain, and the agent reports it. This page shows it four
times over, which is deliberate:

- a **verify failed** pill on the row, beside the role rather than folded into
  it — a replica that failed verification still carries an ordinary role and an
  ordinary age, and this is the only thing on the row saying its contents are
  wrong rather than old;
- a dated warning under the row's contents line, naming the peer it was
  compared against and saying that vmsync refuses every sync into it while the
  finding stands — the **Full resync** offered here included — so the repair is
  a sync set to *recopy once, then re-verify*;
- a warning on the promote cell **outside** the collapsed control, repeated as
  a pill on the control's own summary. Everything else in that cell collapses
  so a row stays one line at rest, but a caution that has to be opened to be
  read is one a hurried operator has already clicked past;
- and the same pill and date on the **source's** row, because that is where
  **Full resync** and **Force clean resync** are offered. Force clean is the
  one control vmsync lets past the finding, and it does not repair the copy —
  it drops the record without re-verifying — so its note says so where the
  operator is about to click it.

Open the control and the form says the rest: vmsync refuses the promotion on
its own, only **force** gets past that refusal, and ticking it changes what
vmsync allows, not what the copy contains.

**The Promote button is not withdrawn.** That is the point, not an oversight.
This console publishes instructions and enforces nothing (see *What it does,
and what it deliberately cannot*), so withdrawing the button would not prevent
the promotion — it would only hide it from the one place that explains what is
wrong, while `vmsync -promote -force-promote` on the hypervisor stays exactly
as available. The console reports and warns; the engine refuses.

### A copy that has served live

The one place this console does withdraw controls, and the reasoning is the
mirror image of the paragraph above: these controls would be **refused by the
engine**, so offering them can only produce a failed operation the operator had
no way to predict.

A VM that was failed over to carries `last_promoted_at`, and that field survives
every role change — which matters because every route out of `promoted` rewrites
the role. Shutting the copy down records `paused`. A fence records `fenced`.
Either way the promotion record goes in the same write, and an hour later the row
looks like an ordinary idle replica: same role, same checkpoint, same recent
sync, same zero failure count. That is the state in which this console used to
offer **Roll back** over it, and **Force clean resync** over it from the
source's row, with nothing between the click and a full overwrite.

So while the record is set, the row shows a **has served live** pill (only once
the role has stopped saying `promoted` — while it still says so, the row already
says it in the present tense) and a warning explaining what is refused, and:

- **Roll back** is withheld;
- **Full resync** and **Force clean resync** are withheld on the *source's* row,
  with the warning shown there too — that is where they are fired from, and the
  record is on the other end;
- **Set role** stays available, but `target` is dropped from its menu. `source`
  and `paused` destroy nothing, and `source` is how you *keep* this copy.

The way out is printed, not offered:

```
vmsync -target-uri qemu:///system -target-domain web01 -release-promotion
```

There is deliberately no button for it, no operation kind, and no field on an
operation that carries it. Everything else vmsync guards can be satisfied from
this console; this is the one that asks for a person at a shell on the host
holding the data. If the failover stands, **Invert** is the alternative that
keeps the data — and it now works even after the copy has been shut down and
demoted, because the same record is what tells vmsync that end really did serve.

**Every action is also re-checked server-side when it is submitted.** The
predicates that decide what a row offers are evaluated again, against state read
at that moment, before an operation is created — so a tab left open across a
failover, a back button, a re-submitted POST or a hand-made `curl` cannot issue
an action the page would no longer draw. This does not make the console an
enforcement point (the engine is still the only one); it makes the console stop
promising things the engine will refuse.

### A replica left half-written by an interrupted rebuild

A full copy into an existing target — `-reinit`, a force-clean, an ordinary
full sync — renames the replica's good disks to
`<disk>.vmsync-replaced-<stamp>` and writes new base images in their place,
with no overlay, while the target domain keeps its **old** metadata. Only a
run that finishes updates that metadata. So a run killed part way — a dropped
link, a restarted agent, a power loss — leaves a domain whose disks are a
truncated copy and whose `last_checkpoint`, `last_sync` and `failure_count`
all describe the complete copy it renamed aside.

Nothing else on this page moves. The role is unchanged, the status is
unchanged, and **`contents as of` is computed from that stale record**, so the
one figure a promoting operator reads as the data-loss window is not about the
disks at all. vmsync arms `replica_incomplete` on the domain before it starts
writing and clears it in the same metadata write that records success, and
this page shows what survives:

- a **partial copy** pill on the replica's row, beside the role like the
  verification one and for a harder reason — that marker at least moves the
  status word, and this one moves nothing;
- a dated explanation under the row naming the verb that armed it, when that
  run started, which host it was writing to, and the `.vmsync-replaced-<stamp>`
  suffix the **complete** copy may still be sitting under. That last part is
  the actionable half: nothing removes those files for you, and they are
  usually the last whole copy of the VM on that host;
- the same pill and explanation on the **source's** row, because **Full
  resync** is offered there and is the only repair — the marker is cleared by
  the same write that records a rebuild succeeding, so nothing but a run that
  finishes clears it. The source's own metadata records none of this: it does
  not know the copy it wrote died half way;
- and a warning on the promote cell, **outside** the collapsed control and
  repeated as a pill on its summary. Open it and the form says vmsync will
  refuse the promotion, that only **force** gets past that refusal, and that
  forcing changes what vmsync allows rather than how much of the copy exists.
  It points at the displaced disks first: promoting the complete older copy by
  putting those files back loses a *known* amount of data, while promoting the
  partial one loses an unknown amount.

**An unreadable value still warns.** The value is a single comma-separated
`k=v` line, parsed here only for display, and a verb from a newer vmsync — or
a corrupted value — renders the warning anyway with the agent's own text shown
verbatim beside it. The absence of this marker is what reads as *safe to
promote*, so it may only ever fail towards the alarm. Unknown keys are ignored
rather than poisoning the parse, which is what stops the first key a newer
engine adds from turning every affected row into an unexplained blob.

**The refusal is not this console's.** It lives in vmsync, on the host holding
the disks, because `-promote` and `-restore` run locally on the target and
during a real disaster the source host is gone — anything a refusal depends on
has to be readable on the survivor. This page reports and explains; see
*Upgrade order* for why every engine that drives syncs must be upgraded before
that refusal can be relied on.

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
