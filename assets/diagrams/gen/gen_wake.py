"""redisq-wake.svg: two consumers wait in the :idle set; an enqueue wakes one, the busy one stays out of :idle,
so the next enqueue wakes the free one at once.

The timeline is simulated with heapq from the real rules: Enqueue = RPUSH + wake(n) (distributed.go:9-11,
notification.go:54-67), nudge (notification.go:74-77, 125-149), varmq's busy gate (varmq/worker_eventloop.go:26),
Len() marking a consumer idle (notification.go:45-47, 184-186), a pop clearing it (distributed.go:60, 72, 82), and
Len() at job end (varmq/worker_pool.go:151, 166). Facts and invariants: STORYBOARD.md, section 2. Line numbers are
those of this branch, as in the storyboard.
"""
import heapq
import itertools
import math

import kfu
from kfu import at, bar, flash, kf, states

CONS = ("C1", "C2")
PHASE = {"C1": .3, "C2": .8}   # nudge ticks at phase + k (notification.go:126, a 1 s ticker per consumer)
ENQ = [(.5, "X"), (1.85, "Y")]  # times the Enqueue script runs (RPUSH + wake); Y comes after C1's 1.3 nudge, and
                                # its slowed wake and pop to C2 end before C2's 2.8 nudge, as they do in real time
                                # (asserted in build: no event inside a flight)
SPOP_PICK = ["C1", "C2"]       # SPOP is random in Redis; this run draws the storyboard's pick per SPOP call
WORK = 2.0                     # seconds a job runs
WAKE_V, POP_V = 1200, 900      # px/s: PUBLISH and pop round trips, slowed down to be visible
LOCAL = .04                    # wake/poke → Len() → pop script (local hand-off, drawn as a short gap)
ITEM_T = .4                    # an enqueued item's walk from the producer to the list
HORIZON = 12.0                 # simulate past the loop end to prove the system stays calm

# geometry (template wake.tpl.svg)
X0 = {"C1": 600, "C2": 776}                       # consumer boxes, y 224..320
PORT = {c: X0[c] + 24 for c in CONS}              # where the wake (top) and the pop (bottom) edges enter
CHIP_X = {"C1": 240, "C2": 356}                   # chip centres in :idle (y 173)
PROD, HEAD_X, SLOT_Y = (100, 326), 392, 326       # item squares: top-left corners (emerge at the box edge, x=116)
SLOT = {0: 392, 1: 356, 2: 320}


def wake_route(c):
    return [(440, 173), (PORT[c], 173), (PORT[c], 222)]


def pop_route(c):
    return [(440, 341), (PORT[c], 341), (PORT[c], 322)]


def plen(pts):
    return sum(abs(x1 - x0) + abs(y1 - y0) for (x0, y0), (x1, y1) in zip(pts, pts[1:]))


def walk(name, t, pts, v, end_op=0, hold=0):
    """Dot along pts at v px/s: fades in at the start, at the end fades out (end_op=0: into the box). -> arrival."""
    out, (x, y) = [at(t, *pts[0], 0), at(t + .04, *pts[0], 1)], pts[0]
    t += .04
    for nx, ny in pts[1:]:
        t += (abs(nx - x) + abs(ny - y)) / v
        x, y = nx, ny
        out.append(at(t, x, y, 1))
    if hold:
        out.append(at(t + hold, x, y, 1))
    out.append(at(t + hold + .12, x, y, end_op))
    kf(name, out)
    return t


def simulate():
    """Event-driven run of the real rules. Returns the logs; all times are drawn times."""
    ev, seq = [], itertools.count()

    def push(t, kind, *a):
        heapq.heappush(ev, (round(t, 4), next(seq), kind, a))

    queue, idle = [], set(CONS)          # start: Subscribe added both (notification.go:164-171)
    busy = {c: None for c in CONS}       # varmq busyWorkers per consumer (concurrency 1): set at dispatch
    free = {c: True for c in CONS}       # Notification.idle: its own last Len()/pop found no work for it
    picks = iter(SPOP_PICK)
    log = {k: [] for k in ("enq", "wake", "nudge", "signal", "pop", "run", "member", "stay", "states")}

    def member(t, c, now_in, why):
        if (c in idle) != now_in:
            log["member"].append((t, c, now_in, why))
        (idle.add if now_in else idle.discard)(c)

    def pull(t, c):
        """varmq event loop with a free slot: pending() → Len() (lenIdle: SADD when 0), then a pop."""
        if not queue:
            member(t, c, True, "len")
            free[c] = True
            return
        busy[c] = "dispatching"                     # busyWorkers.Add(1) before the dequeue
        push(t + LOCAL, "pop", c)

    def signal(t, c, kind):
        """notifyToPullNextJobs → event loop: nothing while busy == concurrency (worker_eventloop.go:26)."""
        if busy[c] is not None:
            log["signal"].append((t, c, kind, "ignored"))
            return
        log["signal"].append((t, c, kind, "taken"))
        pull(t, c)

    for t, job in ENQ:
        push(t, "enqueue", job)
    for c in CONS:
        push(PHASE[c], "nudge", c)

    while ev:
        t, _, kind, a = heapq.heappop(ev)
        if t > HORIZON:
            break
        if kind == "enqueue":                       # RPUSH, then wake(KEYS[2], prefix, n) in one script
            job = a[0]
            queue.append(job)
            n, before, woken = len(queue), set(idle), []
            while len(woken) < n:                   # notification.go:57-62
                if not idle:
                    break
                c = next(picks)
                assert c in idle, f"SPOP can only return a member: {c} not in {idle}"
                member(t, c, False, "spop")         # SPOP removes it before the PUBLISH
                arrive = round(t + plen(wake_route(c)) / WAKE_V, 4)
                log["wake"].append((t, c, arrive, job))
                woken.append(c)                     # c is subscribed: PUBLISH returns 1
                push(arrive, "signal", c, "wake")
            log["enq"].append((t, job, n, woken, before))
        elif kind == "nudge":                       # notification.go:74-77, then poke when items wait (145-146)
            c = a[0]
            n, was_busy = len(queue), busy[c] is not None
            if n > 0:
                member(t, c, False, "nudge")
            elif free[c]:
                member(t, c, True, "nudge")
            elif c not in idle:
                log["stay"].append((t, c))          # busy with work it popped: not put back
            log["nudge"].append((t, c, n, was_busy, free[c], c in idle))
            if n > 0:
                signal(t, c, "poke")
            push(t + 1, "nudge", c)
        elif kind == "signal":
            signal(t, *a)
        elif kind == "pop":                         # DequeueWithAckId: LPOP into pending (queue.go popOneTail)
            c = a[0]
            if not queue:                           # a race: the pop finds nothing → SADD (queue.go:494-496)
                member(t, c, True, "pop-miss")
                free[c] = True
                busy[c] = None
                continue
            job = queue.pop(0)
            free[c] = False                         # a pop that returns a job clears the flag (distributed.go:72)
            busy[c] = job
            arrive = round(t + plen(pop_route(c)) / POP_V, 4)
            log["pop"].append((t, c, job, arrive))
            log["run"].append((c, job, arrive, arrive + WORK))
            push(arrive + WORK, "done", c, job)
        elif kind == "done":                        # job end: freePoolNode → pending() → Len(); then notify
            c, job = a
            busy[c] = None
            pull(t, c)
        log["states"].append((t, kind, tuple(queue), frozenset(idle), tuple(busy[c] for c in CONS)))
    return log


def build():
    kfu.rules.clear()
    kfu.elements.clear()
    log = simulate()
    E = kfu.elements

    # ---- loop length: end state reached (last membership change, last job end) + a calm 1.5 s, whole seconds ----
    settle = max([t for t, *_ in log["member"]] + [e for *_, e in log["run"]])
    kfu.D = D = float(math.ceil(settle + 1.5))
    calm = [s for s in log["states"] if s[0] >= settle]
    assert calm and all(s[2:] == ((), frozenset(CONS), (None, None)) for s in calm), "calm after settle, to HORIZON"
    assert all(t < D - 1.5 + 1e-9 for t, *_ in log["member"])

    # ---- invariants (STORYBOARD.md section 2) ----
    for t, job, n, woken, before in log["enq"]:
        # wakes per enqueue ≤ items waiting after the push and ≤ :idle members (distributed.go:9-11, notification.go:57-60)
        assert len(woken) <= n and len(woken) <= len(before), (t, job, n, woken, before)
    for t, c, arrive, job in log["wake"]:
        # SPOP removes the member before the PUBLISH (notification.go:58)
        assert any(mt == t and mc == c and not mi and why == "spop" for mt, mc, mi, why in log["member"]), (t, c)
    for t, c, n, was_busy, free, in_idle in log["nudge"]:
        # a nudge SREMs and pokes while items wait; with the queue empty it SADDs only a consumer whose own last
        # Len() or pop found no work (notification.go:74-77)
        if n > 0:
            assert not in_idle, (t, c, n)
        elif free:
            assert in_idle, (t, c)
        assert (n > 0) == any(st == t and sc == c and k == "poke" for st, sc, k, _ in log["signal"]), (t, c)
    busy_spans = {c: [(p, a + WORK) for p, cc, _, a in log["pop"] if cc == c] for c in CONS}   # pop → job end
    assert not [(t, c) for t, c, now_in, why in log["member"] if now_in
                and any(s <= t < e for s, e in busy_spans[c])], "no consumer rejoins :idle while it runs a job"
    assert (1.3, "C1") in log["stay"], "C1's nudge at 1.3 leaves it out of :idle while it runs X"
    for t, c, kind, res in log["signal"]:
        # a busy consumer would ignore a wake or a poke (varmq/worker_eventloop.go:26); none reaches one here
        assert res == "taken" and not any(s <= t < e for s, e in busy_spans[c]), (t, c, kind, res)
    for c in CONS:                                  # one job at a time (concurrency 1)
        sp = sorted(busy_spans[c])
        assert all(e <= s2 for (_, e), (s2, _) in zip(sp, sp[1:])), sp
    wake_y = next(w for w in log["wake"] if w[3] == "Y")
    enq_y = next(e for e in log["enq"] if e[1] == "Y")
    assert enq_y[4] == {"C2"} and wake_y[1] == "C2", "busy C1 is out of :idle, so Y's wake can only reach C2"
    pops = [(job, c) for _, c, job, _ in log["pop"]]
    assert pops == [("X", "C1"), ("Y", "C2")], pops    # every job popped once
    enq_t = dict((job, t) for t, job in ENQ)
    pop_y = next(p for p in log["pop"] if p[2] == "Y")
    for t, c, job, _ in log["pop"]:
        # each job is popped through its own wake, at once, not by a nudge a second later
        assert any(abs(st + LOCAL - t) < 1e-6 and sc == c and k == "wake" for st, sc, k, _ in log["signal"]), job
        assert t - enq_t[job] < .5, (job, t)
    assert log["states"][-1][2:] == ((), frozenset(CONS), (None, None)), "end: queue empty, both chips in, both free"
    # the membership story the <desc> tells: C1 SPOPped for X and kept out by its nudges while busy; C2 SPOPped
    # for Y; each back by Len() at its job end
    assert [(c, now_in, why) for _, c, now_in, why in log["member"]] == [
        ("C1", False, "spop"), ("C2", False, "spop"), ("C1", True, "len"), ("C2", True, "len")], log["member"]
    assert [(c, k, r) for _, c, k, r in log["signal"]] == [("C1", "wake", "taken"), ("C2", "wake", "taken")]
    # slowed round trips must not reorder events: in real time a PUBLISH or a pop reaches the consumer within
    # milliseconds, so no nudge of that consumer and no enqueue may fall inside a drawn flight (publish or the
    # signal that led to the pop → drawn arrival)
    flights = ([(t, a, c, f"wake {job}") for t, c, a, job in log["wake"]]
               + [(t - LOCAL, a, c, f"pop {job}") for t, c, job, a in log["pop"]])
    for s, e, c, what in flights:
        assert not [t for t, cc, *_ in log["nudge"] if cc == c and s < t < e], (what, c, s, e)
        assert not [t for t, _ in ENQ if s < t < e], (what, s, e)
    assert SPOP_PICK == [c for _, c, _, _ in log["wake"]] == ["C1", "C2"], "the scenario the note and <desc> tell"

    # ---- items: RPUSH from the producer into the list, popped from the head ----
    for i, (t, job) in enumerate(ENQ):
        p = next(p for p in log["pop"] if p[2] == job)
        idx = 0                                      # list is empty at each push in this run
        assert next(e for e in log["enq"] if e[1] == job)[2] == idx + 1
        E.append(f'<g id="item{job}" class="a"><rect class="item" x="0" y="0" width="30" height="30" rx="5"/>'
                 f'<text class="itxt" x="15" y="15">{job}</text></g>')
        kf(f"item{job}", [at(t - ITEM_T, *PROD, 0), at(t - ITEM_T + .1, PROD[0] + 12, PROD[1], 1),
                          at(t, SLOT[idx], SLOT_Y, 1), at(p[0], HEAD_X, SLOT_Y, 1), at(p[0] + .15, HEAD_X + 8, SLOT_Y, 0)])
    flash("enq-glow", [(t - ITEM_T, t) for t, _ in ENQ], .25)

    # ---- wake: chip leaves at SPOP, pink dot to the consumer's channel ----
    first_wake = {}
    for i, (t, c, arrive, job) in enumerate(log["wake"]):
        E.append(f'<path id="whl{i}" class="a shl" d="M440 173H{PORT[c]}V222"/>')
        flash(f"whl{i}", [(t, arrive)], .55, rise=.06, fall=.3)
        E.append(f'<circle id="wdot{i}" class="a lin sdot" cx="0" cy="0" r="6"/>')
        walk(f"wdot{i}", t - .04, wake_route(c), WAKE_V)
        first_wake.setdefault(job, (t, c, arrive))
    enqs = log["enq"]
    for j, (t, job, n, woken, _) in enumerate(enqs):
        txt = f"wake({n}): SPOP → {', '.join(woken) or 'none'}, PUBLISH"
        E.append(f'<text id="wtxt{job}" class="a ptxt" x="311" y="280">{txt}</text>')
        end = min(t + 1.2, enqs[j + 1][0] - .3) if j + 1 < len(enqs) else t + 1.2
        assert end > t + .5 and (j + 1 == len(enqs) or end + .25 <= enqs[j + 1][0]), "one wake caption at a time"
        flash(f"wtxt{job}", [(t, end)], 1)

    # ---- pop: green dot from the head into the consumer ----
    for t, c, job, arrive in log["pop"]:
        E.append(f'<circle id="pdot{job}" class="a lin dot" cx="0" cy="0" r="7"/>')
        got = walk(f"pdot{job}", t - .04, pop_route(c), POP_V)     # fades in on the way out, like the wake dot
        assert abs(got - arrive) < .01, (job, got, arrive)            # drawn arrival == simulated arrival

    # ---- :idle chips and what changed them ----
    LABEL = {"spop": ("SPOP", "ptxt", "pink"), "nudge": (None, "note", "purple"), "len": ("Len() = 0: SADD", "ftxt", "fg")}
    for c in CONS:
        pts, cur = [(0, {"opacity": 1})], 1
        events = [(t, now_in, why) for t, cc, now_in, why in log["member"] if cc == c]
        for t, now_in, why in events:
            pts += [(t, {"opacity": cur}), (t + .2, {"opacity": 1 if now_in else 0})]
            cur = 1 if now_in else 0
        assert cur == 1, f"{c}'s chip ends in :idle"
        kf(f"chip{c}", pts)
        labels, hls = {}, {}
        stays = [t for t, cc in log["stay"] if cc == c]
        starts = sorted([t for t, *_ in events] + stays)   # one chip label at a time
        for t, now_in, why in events:
            text, cls, colour = LABEL[why]
            text = text or ("nudge: SADD" if now_in else "nudge: SREM")
            nxt = min([u for u in starts if u > t], default=t + 9)
            labels.setdefault((text, cls), []).append((t, min(t + 1.1, nxt - .3)))
            hls.setdefault(colour, []).append((t, t + .35))
        for t in stays:
            # a nudge that leaves the busy consumer out: the label stays until it rejoins (Len() at job end)
            back = min(mt for mt, now_in, _ in events if mt > t and now_in)
            labels.setdefault(("busy: stays out", "note"), []).append((t, back - .3))
            hls.setdefault("purple", []).append((t, t + .35))
        for k, ((text, cls), spans) in enumerate(labels.items()):
            E.append(f'<text id="cl{c}{k}" class="a {cls}" x="{CHIP_X[c]}" y="205">{text}</text>')
            flash(f"cl{c}{k}", spans, 1)
        for colour, spans in hls.items():
            E.append(f'<rect id="hl{c}{colour}" class="a hl-{colour}" x="{CHIP_X[c] - 33}" y="157" width="66" '
                     f'height="32" rx="16"/>')
            flash(f"hl{c}{colour}", spans, .9, rise=.06, fall=.3)

    # ---- consumers: busy glow + bar, status, nudge pulses, poke and refusal labels ----
    for c in CONS:
        runs = [(s, e) for cc, _, s, e in log["run"] if cc == c]
        flash(f"glow{c}", runs, .14)
        bar(f"bar{c}", runs)
        jobs = [j for cc, j, *_ in log["run"] if cc == c]
        E.append(f'<text id="st{c}0" class="a st" x="{X0[c] + 80}" y="273">free</text>')
        for k, j in enumerate(jobs, 1):
            E.append(f'<text id="st{c}{k}" class="a st" x="{X0[c] + 80}" y="273">runs {j}</text>')
        ch = [(0, 0)]
        for k, (s, e) in enumerate(runs, 1):
            ch += [(s, k), (e, 0)]
        states(f"st{c}", ch, len(jobs) + 1)
        pulse(f"ring{c}", PHASE[c])
        pokes = [t for t, cc, n, *_ in log["nudge"] if cc == c and n > 0]
        if pokes:
            # the poke is local (an in-process broadcast after the nudge script, notification.go:145-146): label it
            # inside the box, beside the nudge ring, on a box-coloured tag so it keeps contrast over the busy glow
            x = X0[c] + 136                                  # ring at X0 + 148, y 306 (wake.tpl.svg)
            E.append(f'<g id="poke{c}" class="a"><rect class="tag" x="{x - 78}" y="298" width="81" height="16" '
                     f'rx="4"/><text class="note" x="{x}" y="306" style="text-anchor:end">nudge: poke</text></g>')
            flash(f"poke{c}", [(t, t + .5) for t in pokes], 1)         # gone before this consumer's next nudge

    still = round((wake_y[0] + wake_y[2]) / 2, 2)   # Y's wake on its way to C2, busy C1 out of :idle
    assert wake_y[0] + .1 < still < wake_y[2] - .1
    assert any(s <= still < e for s, e in busy_spans["C1"]) and max(t for t, c in log["stay"] if c == "C1" and t < still)
    spop_note = f"SPOP is random; this run draws {SPOP_PICK[0]} first"
    tpl = open("wake.tpl.svg").read().replace("{DESC}", desc(log)).replace("{SPOPNOTE}", spop_note)
    kfu.render(tpl, "../redisq-wake.svg", still=still)
    return log, still


def pulse(name, phase, length=.6, peak=.9, grow=2.4):
    """Nudge pulse at phase + k for every k, as a seamless train over the loop: a pulse crossing D is split
    into its head before D and its tail after 0, so the keyframes stay inside [0, D] and t=0 equals t=D."""
    D = kfu.D
    assert D == int(D), "whole-second loop: the 1 s pulse train repeats exactly"
    pts = []
    for k in range(-1, int(D) + 1):
        t = phase + k
        pts += [(t, 0, 1), (t + .08, peak, 1.25), (t + length, 0, grow)]

    def f(x):
        for (a, oa, sa), (b, ob, sb) in zip(pts, pts[1:]):
            if a <= x <= b:
                u = (x - a) / (b - a)
                return oa + u * (ob - oa), sa + u * (sb - sa)
        raise AssertionError(x)

    keep = [(0, *f(0))] + [p for p in pts if 0 < p[0] < D] + [(D, *f(D))]
    assert all(abs(u - v) < 1e-9 for u, v in zip(f(0), f(D))), "pulse train seam"
    kf(name, [(round(t, 3), {"opacity": f"{o:.3f}", "transform": f"scale({s:.3f})"}) for t, o, s in keep])


def desc(log):
    return (
        "Two consumers, C1 and C2, each a DistributedQueue with a varmq worker running one job at a time, wait on "
        "one Redis list. At the start the queue is empty, both consumers are free, and both are members of the "
        ":idle set, which Subscribe joined them to. Every consumer runs a nudge once a second: when items wait it "
        "removes itself from :idle (SREM) and pokes its own worker, and when the queue is empty it adds itself back "
        "(SADD), but only if its own last look at the queue found no work for it. Job X is enqueued: one script "
        "pushes it (RPUSH) and wakes one idle consumer, because one item waits. SPOP takes a random member of "
        ":idle, here C1, and PUBLISH sends the wake on C1's own channel. C1 pops X at once and runs it. C1's next "
        "nudges find the queue empty, but C1 popped work, so they leave it out of :idle while it is busy. Job Y "
        "is enqueued: C2 is the only member of :idle, so SPOP takes C2, and C2 pops Y at once. C1 finishes X; its "
        "Len() call finds the queue empty and puts it back in :idle. C2 finishes Y and rejoins the same way. At the "
        "end the queue is empty, both consumers are free, and both are in :idle. Each job was popped once, by the "
        "consumer its wake reached. Redis round trips are slowed down; the nudge period is the real one second."
    )


if __name__ == "__main__":
    log, still = build()
    print("D", kfu.D, "still", still)
    rows = []
    for t, job, n, woken, before in log["enq"]:
        rows.append((t, f"Enqueue {job}: RPUSH, n={n}; :idle was {sorted(before)}; SPOP → {woken} → PUBLISH"))
    for t, c, arrive, job in log["wake"]:
        rows.append((arrive, f"wake for {job} reaches {c}"))
    for t, c, n, was_busy, free, in_idle in log["nudge"]:
        if t < kfu.D:
            act = "SREM + poke" if n else "SADD" if free else "nothing (its last pop found work)"
            rows.append((t, f"nudge {c}: queue len {n} → {act}{' (busy)' if was_busy else ''}; in :idle={in_idle}"))
    for t, c, kind, res in log["signal"]:
        rows.append((t + 1e-4, f"  {kind} at {c}: {res}"))
    for t, c, job, arrive in log["pop"]:
        rows.append((t, f"{c} pops {job} (arrives {arrive:.3f}, runs to {arrive + WORK:.3f})"))
    for c, job, s, e in log["run"]:
        rows.append((e, f"{c} done {job}"))
    for t, c, now_in, why in log["member"]:
        rows.append((t + 2e-4, f"  :idle {'+' if now_in else '-'}{c} ({why})"))
    for t, text in sorted(rows):
        print(f"{t:7.3f}  {text}")
