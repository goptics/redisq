# redisq README diagrams: storyboard

Four animated SVGs for the README, built from `gen_<slug>.py` + `<slug>.tpl.svg` with `kfu.py`
(`make diagrams`). Facts cite the code on `docs/readme-explainers` (on top of `fix/wasted-wakes`, `ad8aa9a`)
and, for consumer behaviour, varmq v1.9.0 (`$GOMODCACHE/github.com/goptics/varmq@v1.9.0`, written `varmq/…`).
Every timeline below is simulated in its generator, and the invariants are `assert`s there.

## Shared vocabulary

Same colour = same role in every diagram (palette from `base.tpl.svg`, GitHub light/dark):

| Role | Colour | Drawn as |
|---|---|---|
| Item waiting in the queue | yellow | rounded square in a list slot |
| Item in flight (popped, not yet acked) | green | dot moving Redis → consumer |
| Pending entry (`:nacked` + deadline) | yellow outline + blue deadline bar | square in the pending box |
| Consumer busy | blue | glow + progress bar on the consumer |
| Wake signal (`PUBLISH <key>:wake:<id>`) | pink | dashed edge + dot |
| Sweep / nudge (once a second, per object) | purple | small pulse |
| Refusal / ignored / dead | red | short mono label (`.why`) |

Layout (960 wide): producer left → Redis box centre (queue list, pending box, `:idle` chips) → consumers right.
Signals run along the top. Times that can't be shown at real speed carry a "times not to scale" note.

## Shared facts

| Fact | Citation |
|---|---|
| A pop moves the item from the list head into `<key>:nacked`, sets its deadline in `<key>:timeouts` and renews the owner's heartbeat, in one script | `queue.go:463-483`, `queue.go:488`, `queue.go:498`, `queue.go:541-543`; batch: `queue.go:388-427` |
| Deadline = Redis `TIME` + visibility timeout (default 5 min) | `queue.go:631-638`, `queue.go:65` |
| `Acknowledge` deletes the pending entry and its deadline | `queue.go:335-355` |
| Each Queue object sweeps once a second; the sweep requeues every item whose deadline passed, to the list front (`LPUSH`), push before delete, in one script | `queue.go:884-885`, `queue.go:807`, `queue.go:747-757` |
| An enqueue pushes and wakes up to *n* idle consumers in one script, *n* = items waiting after the push | `distributed.go:9-11` |
| Wake = `SPOP` a **random** member of `:idle`, `PUBLISH` to its channel; a member with no subscriber is skipped; nothing is sent when `:idle` is empty | `notification.go:52-64` |
| A consumer joins `:idle` on `Subscribe`, when its `Len()` reads 0 (varmq calls it at job end and in its pull loop), when a pop finds nothing (a race), and at its nudge while the queue is empty, if its own last `Len()` or pop found no work | `notification.go:164-171`, `notification.go:45-47`, `notification.go:184-186`, `notification.go:74-77`, `queue.go:476-478`; `varmq/worker_pool.go:166`, `varmq/worker_eventloop.go:26` |
| **Nudge**: once a second per consumer: items waiting → `SREM` itself and poke its own worker locally; queue empty → `SADD` itself to `:idle` only if its own last `Len()` or pop found no work (`Notification.idle`: a pop that returns a job clears it, so a busy consumer stays out) | `notification.go:74-77`, `notification.go:125-149`, `distributed.go:60`, `distributed.go:72`, `distributed.go:82` |
| A busy consumer ignores a wake or poke; a consumer with free slots pops while `Len() > 0`, one `DequeueWithAckId` per free slot (ack sources never batch); a finished job makes it check again | `varmq/worker_eventloop.go:26`, `varmq/worker_eventloop.go:185`, `varmq/source.go:125-136`, `varmq/worker_pool.go:151`, `varmq/source_manager.go:87-91` |
| A requeue of *n* items wakes up to *n* idle consumers (distributed queues only) | sweep `queue.go:912`, Close `queue.go:1023`, `queue.go:917-923`, `distributed.go:29` |
| Each pop that hands out items sets the owner's heartbeat to now + heartbeat timeout (default 30 s); its own sweep renews it and resets its presence mark | `queue.go:647-650`, `queue.go:67`, `queue.go:797-800` |
| Presence: owner with a live heartbeat seen subscribed (mark 0) → a sweep that moves the shared clock and sees no subscriber sets mark = now → a later such sweep with now - mark ≥ 5 s finds it dead | `queue.go:794-795`, `queue.go:811-821`, `queue.go:867` |
| A dead owner (presence, or heartbeat expired) has all its pending items requeued (list front) and its heartbeat and mark dropped | `queue.go:810`, `queue.go:823-833` |
| `DistributedQueue.Close`: leave `:idle`, requeue own pending items, drop own heartbeat and presence, wake | `distributed.go:89-94`, `notification.go:218-221`, `queue.go:996-1025`, `queue.go:840-849` |
| An `Acknowledge` after `Close` fails | `queue.go:337-339` |
| varmq acknowledges a job whose handler failed or panicked | `varmq/main.go:61-79`, `varmq/worker_pool.go:127-131`, `varmq/job.go:291-330` |

## 1. Ack and visibility timeout  (`redisq-ack.svg`)

Question: where is a job between `DequeueWithAckId` and `Acknowledge`, and what if the ack never comes?
Actors: `Enqueue` (producer); Redis box: queue `<key>` (list), pending `:nacked` (each entry with a deadline bar
from `:timeouts`), Sweep (one pulse per second); one consumer. Producer and consumer share one `Queue` object,
so there is one sweeper (each Queue object on a key sweeps; more objects would mean more pulses).
States: item waiting / in flight / pending / acked; consumer idle / busy.
Start state: queue empty, pending empty, consumer idle.
Timeline (simulated; visibility timeout drawn as 4 s, labelled "5 min"; times not to scale):
  0.4 Enqueue A, 0.9 Enqueue B (both wait, A at the head)
  1.4 consumer pops A: A leaves the list head and appears in pending with a deadline bar, in one step;
      a green dot carries A to the consumer; busy
  3.4 consumer acks A: A's entry and bar vanish (tick)
  3.8 consumer pops B (same move) and returns without `Acknowledge` (red "no Acknowledge")
  B's deadline passes 4 s later → the first sweep pulse at/after it moves B back to the list front
  next poll pops B again (new ack ID, new deadline), consumer acks → pending empty
Loop: D = derived: last ack + 1.5
Invariants:
  - an item is in exactly one of {list, pending, done} at every keyframe: pop and requeue are each one script — `queue.go:498`, `queue.go:749-757`
  - the deadline bar spans exactly the visibility timeout from the pop — `queue.go:631-638`
  - ack removes the entry and its deadline — `queue.go:335-355`
  - B returns at the first sweep at or after its deadline (not at the deadline itself), to the list front — `queue.go:884-885`, `queue.go:807`, `queue.go:747`
  - B is delivered twice, A once; end: list and pending empty, consumer idle
  - (no other requeue path fires: one live object, no Close, own sweep keeps its heartbeat alive — `queue.go:796-800`)
Why-labels: "no Acknowledge", "deadline passed"
Still frame (reduced motion): B's deadline bar ~80% full, A acked
Seam check: list empty, pending empty, consumer idle, no dots
Builds on: none (introduces list, pending, deadline, sweep)
README note: varmq acknowledges failed jobs too; under varmq a job reaches this path only by running past the timeout.

## 2. Waking idle consumers  (`redisq-wake.svg`)

Question: when a job arrives, who is told, and why not a busy consumer?
Actors: `Enqueue` (one script: `RPUSH` + wake); Redis box: queue, `:idle` set (chips C1, C2); consumers C1, C2
(`DistributedQueue` + varmq worker, concurrency 1), each with its own wake channel and a nudge tick (once a
second, fixed phase per consumer: C1 at .3 + k, C2 at .8 + k).
States: chip in `:idle` or not; consumer idle / busy.
Start state: queue empty, C1 and C2 idle, both chips in `:idle` (Subscribe added them; nudges keep them there).
Timeline (simulated):
  0.5 Enqueue X: n = 1 → SPOP (random; here C1) → PUBLISH → C1 pops X at once, busy. Its chip left at the SPOP.
  C1's nudges (1.3, 2.3): queue empty, but C1's last pop found work → no SADD: its chip stays out while it runs X
      (purple "busy: stays out")
  1.85 Enqueue Y (after C1's 1.3 nudge; drawn so its slowed wake and pop end before C2's 2.8 nudge):
      n = 1 → `:idle` holds only C2 → SPOP C2 → PUBLISH → C2 pops Y at once; C2's nudges then leave it out too
  C1 finishes X → its `Len()` reads 0 → back in `:idle`; C2 finishes Y → back the same way
Loop: D = ceil(max(last `:idle` change, last job end) + 1.5), whole seconds so the nudge pulses repeat
Invariants:
  - wakes per enqueue ≤ items waiting after the push and ≤ `:idle` members — `distributed.go:9-11`, `notification.go:57-60`
  - SPOP removes the member before the PUBLISH — `notification.go:58`
  - a nudge SREMs + pokes when items wait; with the queue empty it SADDs only a consumer whose own last `Len()` or
    pop found no work — `notification.go:74-77`, `notification.go:140-146`
  - no consumer rejoins `:idle` while it runs a job; no wake or poke reaches a busy consumer — `varmq/worker_eventloop.go:26`
  - each job is popped through its own wake, under half a (drawn) second after its enqueue, not by a nudge
  - every job popped once; end: queue empty, both chips in `:idle`
Why-labels: "busy: stays out"
Still frame: Y's wake on its way to C2, busy C1 out of `:idle`
Seam check: queue empty, C1 and C2 idle, both chips in `:idle`
Builds on: 1, adds consumers that wait for a signal, and the 1 s nudge as fallback
README note: SPOP is random among idle consumers; a busy one is not among them. The nudge (one script per consumer
per second) bounds a lost wake to ≤ 1 s. A wake can still reach a consumer that can't take the job: a paused
worker, or one marked idle by a `Len()` call from elsewhere while it is busy.

## 3. `Close` hands work back  (`redisq-close.svg`)

Question: what happens to a consumer's unacknowledged jobs when it shuts down (deploy, scale-down, Ctrl+C)?
Actors: as 2 plus the pending box (entries tagged with their owner); C2 (concurrency 3) running; C1 a new instance.
States: C1 not started (ghost) / idle / busy; C2 idle / busy / closing / gone; chips in `:idle`.
Start state: queue empty, pending empty, C2 idle (chip in `:idle`), C1 ghost.
Timeline (simulated):
  0.4 Enqueue J1, J2, J3 back to back: J1's push SPOPs C2 and PUBLISHes; J2 and J3 find `:idle` empty (no wake)
  C2 wakes with 3 free slots → three back-to-back `DequeueWithAckId` calls (one per free slot; varmq ack sources
  never batch), each moving one job to pending (tagged C2) in one script; C2 3/3 busy
  C2's nudges: queue empty, but its last pop found work → no SADD: C2's chip stays out while it is full
  2.4 C1 starts: `Subscribe` adds its chip
  3.4 C2 `Close()` with J1-J3 still running and unacknowledged:
      leave() pulls C2's chip out of `:idle` → release script: J1-J3 back to the list front (`LPUSH` in pop order,
      so the list reads J3 J2 J1), C2's heartbeat and presence dropped → wake(3) → SPOP C1 (the only member)
  C1 (concurrency 3) takes J3, J2, J1, one `DequeueWithAckId` each, runs and acks them
  C2's runs are cut off: red "acks after Close fail · runs again on C1"; C2 fades out
  end: C1 idle in `:idle`, C2 gone → hold 1 s, cross-fade to the start state, "↻ replay"
Loop: D = ceil(cross-fade end + .5), whole seconds so the nudge pulses repeat
Invariants:
  - only J1's push wakes (n ≥ 1 but `:idle` empty for J2, J3) — `distributed.go:9-11`, `notification.go:58-59`
  - a woken consumer pops while it has a free slot and `Len() > 0`, one `DequeueWithAckId` per slot — `varmq/source.go:125-136`, `varmq/worker_eventloop.go:26`, `varmq/worker_eventloop.go:185`, `queue.go:463-483`
  - C2 is full, so its nudges leave it out of `:idle`; at Close leave() finds it out, before the release, so wake(3) can only reach C1 — `notification.go:74-77`, `distributed.go:90-91`, `notification.go:218-221`
  - all of C2's pending entries move back in one script, list order J3 J2 J1; heartbeat and presence dropped — `queue.go:840-849`, `queue.go:747`
  - Close → C1's pop involves no timer (well under a second)
  - every job is acked once, by C1; C2's runs were wasted (at-least-once) — `queue.go:337-339`, `queue.go:992-995`
Why-labels: "acks after Close fail · runs again on C1"
Still frame: jobs back in the list, wake on its way to C1
Seam check: cross-fade proves only the reset (C2 is gone; the system does not return to its start)
Builds on: 2, adds pending owners and `Close`
README note: stop or drain workers before Close (varmq `w.Stop()`) so nothing runs twice.

## 4. Detecting a dead consumer  (`redisq-dead.svg`)

Question: a consumer dies holding jobs and never calls `Close`. How long until another consumer gets them, and why?
Actors: two rows sharing one real-seconds clock (0-34 s, drawn scaled, times not to scale):
  Row A, "process killed (kill -9, OOM)"; Row B, "host lost (Redis keeps its socket)".
  Each row: C2 holding 2 pending jobs, its subscription (pink dot on its channel), presence mark, heartbeat bar;
  C1 alive and idle, sweeping once a second.
States: C2 alive / dead; subscription present / gone; mark 0 / "gone since t"; heartbeat bar full → empty.
Start state (both rows): C2 holds 2 pending jobs, heartbeat just renewed (now + 30 s), subscribed and already seen
subscribed by a sweep (mark 0); queue empty; C1 idle.
Timeline (simulated in real seconds; sweep phases fixed so the clock-move rule is exercised):
  0   C2 stops in both rows: no more sweeps, so no heartbeat renewal and no mark reset.
  Row A: Redis drops the subscription at once. The first C1 sweep that moves the shared clock (≥ .5 s since its
         last move) sees no subscriber with mark 0 → mark = now (1.3 as built: C1's sweep at 0.3 doesn't move the
         clock, C2's own last sweep at 0 did). The first such sweep with now - mark ≥ 5 (6.3) → C2 dead → J1, J2
         back to the list front, C2's heartbeat and mark dropped → wake → C1 pops them (≈ 6 s; 5-7.5 s in general)
  Row B: Redis still counts the subscription (the dead host never closed it), so the mark stays 0; the heartbeat
         runs out at 30 s → first C1 sweep after it (30.x) → same requeue and wake (≈ 30-31 s)
  end: both rows hold 1 s, cross-fade to start, "↻ replay"
Loop: D = derived from the scaled 31 s + tail
Invariants:
  - presence: live heartbeat, seen subscribed (mark 0), then a clock-moving sweep with no subscriber sets the mark,
    then a clock-moving sweep with now - mark ≥ 5 s and no own sweep or subscriber in between — `queue.go:794-795`, `queue.go:800`, `queue.go:811-821`, `queue.go:867`
  - heartbeat = last renewal + 30 s; reaped at the first sweep at or after it, ticked or not — `queue.go:67`, `queue.go:647-650`, `queue.go:799`, `queue.go:810`
  - a reap requeues all of C2's items (list front) and drops its heartbeat and mark; the distributed sweeper wakes — `queue.go:823-833`, `queue.go:912`, `distributed.go:29`
  - row A requeues before row B; row A's heartbeat is dropped at the reap, not left to run out
  - a lost host's socket stays open to Redis until TCP notices (minutes), so its subscription still counts — `queue.go:781-782` **(inferred: Redis/TCP behaviour, not redisq code; no duration printed)**
Why-labels: "subscription gone", "socket still open: wait for heartbeat"
Still frame: real t = 11 s: row A requeued and acked, row B still waiting on its heartbeat (before the fast-forward,
clear of the clock readout's fades)
Seam check: cross-fade proves only the reset
Builds on: 3, adds death without Close
README note: a plain `Queue` never subscribes, so it also waits for the heartbeat (≈ 30-31 s) and gets no wake; its
consumers find the items on their next poll.

Not animated, in the README text: the stall guard (no sweep moved the clock for over 2 s: Redis paused, failed
over or restarted, or every process stalled → the next sweep moves every heartbeat and every running presence mark
on by the whole gap, `queue.go:790-793`, `queue.go:862`), at-least-once caveats, timeouts, no redelivery limit,
cost per pop.

## As built

| Diagram | Generator | D (s) | Reduced-motion still (s) |
|---|---|---|---|
| `redisq-ack.svg` | `gen_ack.py` | 17 | 8.55 (B's deadline bar 80% full, "no Acknowledge") |
| `redisq-wake.svg` | `gen_wake.py` | 7 | 2.02 (Y's wake on its way to C2, busy C1 out of `:idle`) |
| `redisq-close.svg` | `gen_close.py` | 11 | 4.15 (jobs back in the list, wake on its way to C1) |
| `redisq-dead.svg` | `gen_dead.py` | 19 | 10.19 (real t = 11 s) |

All four were checked as light and dark contact sheets plus full-size key frames. Each generator asserts its
section's invariants; `make diagrams` rebuilds them.

## Changes

- Checkpoint 1: user approved the 4 concepts; stall guard and trade-offs stay as text. Work lands on a new
  branch `docs/readme-explainers` stacked on PR #21. Diagrams live in `assets/diagrams/` as in varmq (not the
  skill's default `assets/explainers/`).
- Truth check before checkpoint 2 (5 independent verifiers against redisq and varmq source):
  - Diagram 2 rewritten: the 1 s nudge re-adds busy consumers to `:idle`, so a wake can land on a busy consumer
    and be ignored; a free consumer's nudge takes the item within 1 s. "A busy consumer gets no signal" was wrong.
  - Diagram 3: J1-J3 arrive as a burst (one wake, one pop of 3); C2's chip is back in `:idle` (nudge) at Close,
    which is what makes leave() matter.
  - Diagram 1: "returns without Acknowledge" (varmq acks failed jobs); one sweeper stated.
  - Diagram 4: row B is "host lost" only (a plain Queue never subscribes); mark lands within 1.5 s, reap at
    5-7.5 s; no "~10 min" figure.
  - Citations corrected throughout.
- Checkpoint 2: user approved the corrected storyboard.
- Build and review (independent reviewer per diagram, one fix round), behaviour the code decided:
  - Diagram 3: varmq v1.9.0 ack sources never batch (`varmq/source.go:125-136`), so C2 and C1 each pop the three
    jobs with three back-to-back `DequeueWithAckId` calls, not one batch pop. `varmq/worker_credit.go` (cited by a
    verifier) exists only on varmq's unreleased main, not in v1.9.0.
  - Diagram 2: Y drawn at 2.0 (slowed round trips); C2 rejoins `:idle` at its nudge while still busy; D rule
    covers the last job end.
  - Diagram 4: mark at 1.3, reap at 6.3; still moved to real t = 11 s.
  - `notification.go` header comment corrected on this branch; its citations here moved by +2.
- Rebased onto `fix/wasted-wakes` (#23), which changes the nudge: with the queue empty it re-adds only a consumer
  whose own last `Len()` or pop found no work, so a busy consumer stays out of `:idle`.
  - Diagram 2 retold: C1 stays out while it runs X, so Y's wake goes to C2 at once; no wake is ignored.
  - Diagram 3: full C2 stays out of `:idle`; leave() at Close finds it out; C1 rejoins at its first job end.
  - Diagram 4: C2, holding J1 and J2, starts out of `:idle` (not drawn; the wake reaches C1 either way).
  - The `notification.go` header comment fix moved to #23, so this branch changes no Go code; `notification.go`
    and `distributed.go` citations re-mapped.
