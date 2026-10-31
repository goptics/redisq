"""redisq-dead.svg: a consumer dies holding two jobs and never calls Close. Who notices, and when?

Two rows on one real-seconds clock. A: the process is killed, so Redis drops its subscription at once.
B: the host is lost, so Redis keeps its socket and still counts the subscription. In both rows C1, alive and
idle, sweeps once a second. Every sweep runs a port of the sweep script (tickBody, queue.go:806-856), in whole
milliseconds so "now - mark >= 2.5 s" is exact. Facts and invariants: STORYBOARD.md, section 4.
"""
import heapq
import itertools
import math
import re

import kfu
from kfu import at, bar, flash, kf, ring

# ---- the rules, in milliseconds ----
HBT = 30_000          # heartbeat timeout, default 30 s (queue.go:85)
GRACE = 2_500         # presenceGrace (queue.go:891)
GRACE_S = f"{GRACE / 1000:g}"   # as the labels print it
TICK = 500            # a sweep moves the shared clock only if >= .5 s since the last move (queue.go:814)
GAP = 2_000           # sweepGap: a longer gap is a stall (queue.go:810-813, 862); must not fire here
VT = 300_000          # visibility timeout, default 5 min (queue.go:83): out of reach in this story
C2_PHASE, C1_PHASE = 0, 300   # each Queue object sweeps once a second (queue.go:908-909); fixed phases
T0 = -5_000           # history: C2 popped J1, then J2 (varmq pops one per call for ack storage)
KILL = 100            # C2 stops 0.1 s after its own sweep at 0
END = 34_600          # real time drawn (the clock shows 0-34 s)
WORK = 2_000          # C1 runs each job 2 s
CONC = 2              # C1's varmq concurrency
WAKE_MS, POP_MS = 5, 5    # the wake follows the sweep, and each pop the one before, within milliseconds

# ---- drawing ----
R = .7                # drawn seconds per real second outside the slow and fast parts
FF = (9.0, 29.0)      # real seconds fast-forwarded (nothing happens in them)
FF_DRAWN = 2.5
XF = .7               # cross-fade back to the start state
P = {0: (424, 129), 1: (540, 129)}           # pending entries, in pop order
SLOT = {0: (656, 53), 1: (620, 53)}          # list slots, head first
POP_EDGE, WAKE_EDGE = [(702, 68), (776, 68)], [(712, 40), (776, 40)]
ROWS = {"A": dict(r="a", Y=58, title="A · process killed (kill -9, OOM)", kill="✕ killed"),
        "B": dict(r="b", Y=254, title="B · host lost (Redis keeps its socket)", kill="✕ host lost")}
H = 446


class Redis:
    """The keys of one queue <key> that this story touches."""

    def __init__(self):
        self.lst = []          # <key>: list, head first
        self.pending = {}      # <key>:nacked: ack ID -> item; an ack ID starts with its owner's prefix
        self.dl = {}           # <key>:timeouts: ack ID -> visibility deadline
        self.hb = {}           # <key>:heartbeats: owner -> when it runs out
        self.pres = {}         # <key>:presence: owner -> 0 (seen subscribed) or when a sweep first saw none
        self.clock = None      # <key>:heartbeats:clock: when a sweep last moved it
        self.subs = {}         # PUBSUB NUMSUB <key>:wake:<owner>
        self.idle = set()      # <key>:idle
        self.seq = itertools.count(1)

    def pop_one(self, owner, now):
        """listAckPopOne: LPOP the head into pending with a deadline, renew the owner's heartbeat, one script
        (queue.go:539-558, 588-592; beat: queue.go:665-668)."""
        if not self.lst:
            self.idle.add(owner)
            return None
        v = self.lst.pop(0)
        aid = f"{owner}:{next(self.seq):04d}"     # sorts in pop order, as <prefix>:<reservation>:<index> does
        self.pending[aid], self.dl[aid], self.hb[owner] = v, now + VT, now + HBT
        return aid, v

    def ack(self, aid):
        """Acknowledge deletes the entry and its deadline (queue.go:353-373)."""
        del self.pending[aid], self.dl[aid]

    def move(self, aid):
        """moveFn + listRepush: LPUSH to the front, then drop from pending (queue.go:749-775)."""
        v = self.pending.get(aid)
        if v is not None:
            self.lst.insert(0, v)
            del self.pending[aid]
        self.dl.pop(aid, None)
        return int(v is not None)

    def tick(self, me, now):
        """tickBody (queue.go:806-856) for Queue object `me`. Returns (moved, ticked, notes)."""
        notes = []
        last = self.clock
        assert not (last is not None and now - last > GAP), ("stall guard fired", now, last)
        ticked = last is None or now - last >= TICK or now < last
        if ticked:
            self.clock = now
        if me in self.hb:                                      # own heartbeat: renew, presence back to 0
            self.hb[me] = now + HBT
            if me in self.pres:
                self.pres[me] = 0
        n = 0
        assert all(d > now for d in self.dl.values()), "no visibility deadline passes in this story"
        if last is not None:
            dead = {m for m, s in self.hb.items() if s <= now}            # heartbeat ran out (queue.go:830)
            notes += [("heartbeat ran out", m) for m in sorted(dead)]
            if ticked:                                                    # presence (queue.go:831-841)
                for m in sorted(m for m, s in self.hb.items() if s >= now):
                    s = self.pres.get(m)
                    if self.subs.get(m, 0) > 0:
                        if s != 0:
                            self.pres[m] = 0
                    elif s == 0:
                        self.pres[m] = now
                        notes.append(("mark", m))
                    elif s is not None and now - s >= GRACE:
                        dead.add(m)
                        notes.append(("gone", m))
                    elif s is not None:
                        notes.append(("waiting", m))
            if dead:                                                      # reap (queue.go:843-853)
                for aid in sorted(a for a in self.pending if a.split(":")[0] in dead):
                    n += self.move(aid)
                for m in dead:
                    self.hb.pop(m, None)
                    self.pres.pop(m, None)
                notes.append(("reap", tuple(sorted(dead))))
        return n, ticked, notes

    def wake(self, count, order):
        """wakeFn (notification.go:54-65): SPOP members until count are reached; a member whose channel has no
        subscriber is skipped. SPOP is random: `order` is one possible sequence of picks."""
        woken = []
        for m in order:
            if len(woken) >= count or m not in self.idle:
                continue
            self.idle.discard(m)
            if self.subs.get(m, 0) > 0:
                woken.append(m)
        return woken


def simulate(row):
    r = Redis()
    r.subs = {"C1": 1, "C2": 1}            # both subscribed to their own wake channel (notification.go:85)
    r.idle = {"C1"}                        # Subscribe added both; C2 popped J1, J2, so its nudge leaves it out while
    r.lst = ["J1", "J2"]                   # it holds them (notification.go:74-77, 164-171)
    for _ in range(2):
        r.pop_one("C2", T0)
    where = {"J1": [(T0, "pending:C2")], "J2": [(T0, "pending:C2")]}
    ev, seq = [], itertools.count()
    for k in range(T0 // 1000, END // 1000 + 1):
        for who, ph in (("C2", C2_PHASE), ("C1", C1_PHASE)):
            if T0 < k * 1000 + ph <= END:
                heapq.heappush(ev, (k * 1000 + ph, next(seq), "sweep", who))
    heapq.heappush(ev, (KILL, next(seq), "kill", "C2"))
    alive, busy, log, start = {"C1", "C2"}, {}, [], None
    while ev:
        t, _, kind, arg = heapq.heappop(ev)
        if kind == "kill":
            start = dict(hb=dict(r.hb), pres=dict(r.pres), clock=r.clock, lst=list(r.lst), pending=dict(r.pending))
            alive.discard("C2")
            if row == "A":
                r.subs["C2"] = 0           # the connection closed with the process: Redis drops it at once
            log.append((t, "kill", None))
        elif kind == "sweep":
            if arg not in alive:
                continue
            n, ticked, notes = r.tick(arg, t)
            log.append((t, "sweep", dict(who=arg, ticked=ticked, notes=notes, mark=r.pres.get("C2"),
                                         hb=r.hb.get("C2"), lst=list(r.lst))))
            if any(k_ == "reap" for k_, *_ in notes):
                for v in r.lst:
                    where[v].append((t, "list"))
            if n and arg == "C1":          # a DistributedQueue's onRequeue is wake(n) (distributed.go:29, queue.go:950-956)
                heapq.heappush(ev, (t + WAKE_MS, next(seq), "wake", n))
        elif kind == "wake":
            members = sorted(r.idle)
            outcomes = [r.__class__.wake(_copy(r), arg, o) for o in itertools.permutations(members)]
            assert all("C1" in w for w in outcomes), ("C1 is woken whatever SPOP picks", outcomes)
            woken = r.wake(arg, members)
            log.append((t, "wake", dict(count=arg, woken=woken, idle_before=members)))
            free = CONC - len(busy)        # varmq pulls while a slot is free and Len() > 0, one
            for i in range(min(free, len(r.lst))):   # DequeueWithAckId per job (worker_eventloop.go:25-31, source.go:125-129)
                heapq.heappush(ev, (t + POP_MS * (i + 1), next(seq), "pop", "C1"))
        elif kind == "pop":
            aid, v = r.pop_one(arg, t)
            busy[aid] = v
            where[v].append((t, "pending:C1"))
            log.append((t, "pop", dict(item=v, aid=aid)))
            heapq.heappush(ev, (t + WORK, next(seq), "ack", aid))
        elif kind == "ack":
            v = busy.pop(arg)
            r.ack(arg)
            where[v].append((t, "done"))
            log.append((t, "ack", dict(item=v)))
    return r, start, log, where


def _copy(r):
    c = Redis()
    c.idle, c.subs = set(r.idle), dict(r.subs)
    return c


def facts(row, r, start, log, where):
    """Pull the drawn moments out of the log and assert the storyboard's invariants on it."""
    sw = [(t, e) for t, k, e in log if k == "sweep"]
    kill = next(t for t, k, _ in log if k == "kill")
    c1 = [(t, e) for t, e in sw if e["who"] == "C1"]
    reap = next(t for t, e in sw if any(n[0] == "reap" for n in e["notes"]))
    reaper = next(e for t, e in sw if t == reap)
    wake = next((t, e) for t, k, e in log if k == "wake")
    pops = [(t, e["item"]) for t, k, e in log if k == "pop"]
    acks = [(t, e["item"]) for t, k, e in log if k == "ack"]
    c2_last = max(t for t, e in sw if e["who"] == "C2")
    first = next((t, e) for t, e in c1 if t > kill)

    # start state, just before C2 stops: renewed by its own sweep, seen subscribed, queue empty
    assert c2_last == 0 and start["clock"] == 0, "C2's own sweep at 0 moved the shared clock"
    assert start["hb"] == {"C2": HBT} and start["pres"] == {"C2": 0} and start["lst"] == []
    assert sorted(start["pending"].values()) == ["J1", "J2"] and all(a.startswith("C2:") for a in start["pending"])
    # the clock rule is exercised: while C2 lived, its sweeps moved the clock and C1's (.3 s later) never did;
    # C1's first sweep after the kill doesn't move it either, so it runs no presence check
    c2_first = min(t for t, e in sw if e["who"] == "C2")
    assert all(not e["ticked"] for t, e in c1 if c2_first < t < kill)
    assert all(e["ticked"] for t, e in sw if e["who"] == "C2" and t > c2_first)
    assert not first[1]["ticked"] and first[0] - c2_last < TICK and first[1]["notes"] == []
    marks = [t for t, e in sw if ("mark", "C2") in e["notes"]]
    hb_out = start["hb"]["C2"]
    if row == "A":
        mark = marks[0]
        assert len(marks) == 1 and mark == next(t for t, e in c1 if t > kill and e["ticked"]), \
            "mark = now at the first clock-moving sweep with no subscriber"
        assert all(e["mark"] == 0 for t, e in sw if c2_first <= t < mark), "mark 0 from the first check until then"
        assert reap == next(t for t, e in c1 if t > mark and e["ticked"] and t - mark >= GRACE), \
            "dead at the first clock-moving sweep with now - mark >= 2.5 s"
        assert ("gone", "C2") in reaper["notes"] and ("heartbeat ran out", "C2") not in reaper["notes"]
        assert not [t for t, e in sw if e["who"] == "C2" and mark <= t <= reap], "no own sweep in between"
        assert all(e["ticked"] and ("waiting", "C2") in e["notes"] for t, e in c1 if mark < t < reap), \
            "every sweep in between moves the clock and finds no subscriber (one would reset the mark to 0)"
        assert hb_out > reap, "row A's heartbeat still had time left: it is dropped at the reap, not run out"
        assert GRACE <= reap - kill <= GRACE + 2_500, "2.5-5 s"
    else:
        mark = None
        assert not marks and all(e["mark"] == 0 for t, e in sw if c2_first <= t < reap), "subscriber counted: mark stays 0"
        assert reap == min(t for t, e in c1 if t >= hb_out), "first sweep at or after the heartbeat, ticked or not"
        assert ("heartbeat ran out", "C2") in reaper["notes"]
        assert 30_000 <= reap <= 31_000
    assert reaper["who"] == "C1" and reaper["hb"] is None and reaper["mark"] is None, "heartbeat and mark dropped"
    assert reaper["lst"] == ["J2", "J1"], "both items in one sweep, LPUSH in pop order: J2 at the head"
    assert wake[1]["count"] == 2 and "C1" in wake[1]["woken"] and wake[0] - reap < 1000
    assert [v for _, v in pops] == reaper["lst"], "C1 pops from the head, one DequeueWithAckId per job"
    assert len({v for _, v in acks}) == 2 and not r.lst and not r.pending, "end: list and pending empty"
    for v, w in where.items():
        assert [p for _, p in w] == ["pending:C2", "list", "pending:C1", "done"], (v, w)
        assert [t for t, _ in w] == sorted(t for t, _ in w)
    s = lambda ms: ms / 1000
    return dict(kill=s(kill), c2_last=s(c2_last), skip=s(first[0]), mark=s(mark) if mark else None, reap=s(reap),
                wake=s(wake[0]), pops=[(s(t), v) for t, v in pops], acks=[(s(t), v) for t, v in acks],
                hb_out=s(hb_out), order=reaper["lst"], woken=wake[1]["woken"],
                sweeps=[(s(t), e["ticked"], e["notes"]) for t, e in c1 if t > kill])


class TimeMap:
    """Real seconds -> drawn seconds, piecewise linear between knots."""

    def __init__(self, knots):
        assert all(a[0] < b[0] and a[1] < b[1] for a, b in zip(knots, knots[1:])), knots
        self.k = knots

    def __call__(self, s):
        k = self.k
        if s <= k[0][0]:
            return k[0][1]
        for (r0, d0), (r1, d1) in zip(k, k[1:]):
            if s <= r1:
                return d0 + (s - r0) * (d1 - d0) / (r1 - r0)
        raise ValueError(s)


def build_map(fa, fb):
    """Drawn time: a calm .4 s at real 0, slow through the kill and the first two sweeps after it, R elsewhere,
    each reap's few milliseconds (requeue, wake, two pops) stretched to 1.5 s, and FF squeezed to FF_DRAWN."""
    assert fa["skip"] == fb["skip"] and fa["kill"] == fb["kill"], "one clock, one sweep phase"
    k = [(0.0, 0.4), (fa["kill"], 1.0), (fa["skip"], 1.8), (fa["mark"], 3.0)]

    def lin(s, rate=R):
        k.append((s, k[-1][1] + (s - k[-1][0]) * rate))

    def slow(f):
        for s in (f["wake"], f["pops"][0][0], f["pops"][1][0]):
            k.append((s, k[-1][1] + .5))

    lin(fa["reap"])
    slow(fa)
    lin(FF[0])
    k.append((FF[1], k[-1][1] + FF_DRAWN))
    lin(fb["reap"])
    slow(fb)
    lin(END / 1000)
    return TimeMap(k)


# ---- drawing helpers ----
XF0 = XFM = XF1 = 0.0


def vis(name, spans, peak=1, fade=.15):
    """Opacity 0 / peak over spans (s, e). s None: shown at the loop start (faded back in over the cross-fade);
    e None: shown until the cross-fade fades it out."""
    on0, on_end = any(s is None for s, _ in spans), any(e is None for _, e in spans)
    pts = [(0, {"opacity": peak if on0 else 0})]
    for s, e in sorted(spans, key=lambda p: -1 if p[0] is None else p[0]):
        if s is not None:
            pts += [(s, {"opacity": 0}), (s + fade, {"opacity": peak})]
        if e is not None:
            pts += [(e, {"opacity": peak}), (e + fade, {"opacity": 0})]
    if on_end and not on0:                    # the end state leaves over the first half of the cross-fade
        pts += [(XF0, {"opacity": peak}), (XFM, {"opacity": 0})]
    if on0 and not on_end:                    # the start state comes back over the second half
        pts += [(XFM, {"opacity": 0}), (XF1, {"opacity": peak})]
    kf(name, pts)


def swap(prefix, changes, n, fade=.15):
    """kfu.states with a fade per change: changes [(t, state)] or [(t, state, fade)], from t=0. A change at XF0
    is the cross-fade: the old text leaves over its first half, the start state's text comes in over the second,
    so the two never overlap."""
    changes = sorted(changes, key=lambda c: c[0])
    assert changes[0][0] == 0
    for k in range(n):
        pts, cur = [], None
        for c in changes:
            t, i, f = c[0], c[1], c[2] if len(c) > 2 else fade
            on = int(i == k)
            if cur is None:
                pts.append((0, {"opacity": on}))
            elif on != cur and t == XF0:
                pts += [(XF0, {"opacity": 1}), (XFM, {"opacity": 0})] if cur else [(XFM, {"opacity": 0}), (XF1, {"opacity": 1})]
            elif on != cur:
                pts += [(t, {"opacity": cur}), (t + f, {"opacity": on})]
            cur = on
        kf(f"{prefix}{k}", pts)


def walk(name, t, pts, v=300):
    """Dot along pts at v px/s, visible on the edge, hidden at the end. Returns arrival time."""
    out, (x, y) = [at(t, *pts[0], 0), at(t + .05, *pts[0], 1)], pts[0]
    t += .05
    for nx, ny in pts[1:]:
        t += (abs(nx - x) + abs(ny - y)) / v
        x, y = nx, ny
        out.append(at(t, x, y, 1))
    out.append(at(t + .05, x, y, 0))
    kf(name, out)
    return t


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def text(id_, x, y, cls, s):
    return f'<text id="{id_}" class="a {cls}" x="{x}" y="{y}">{esc(s)}</text>'


def draw_row(row, f, M):
    """Keyframes and generated state texts for one row; returns the markup for <!--ROW-ELEMENTS-->."""
    r = ROWS[row]["r"]
    els = []
    kill_d, c2s_d = M(f["kill"]), M(f["c2_last"])
    reap_d, wake_d = M(f["reap"]), M(f["wake"])
    (p1, v1), (p2, v2) = [(M(t), v) for t, v in f["pops"]]
    ack_d = {v: M(t) for t, v in f["acks"]}
    sweeps = [(M(t), t, tk, notes) for t, tk, notes in f["sweeps"]
              if not FF[0] < t < FF[1] and M(t) + .6 <= XF1]

    # C2: alive and busy, then stopped
    kf(f"c2-{r}", [(0, {"opacity": 1}), (kill_d, {"opacity": 1}), (kill_d + .4, {"opacity": .35}),
                   (XF0, {"opacity": .35}), (XF1, {"opacity": 1})])
    kf(f"c2glow-{r}", [(0, {"opacity": .14}), (kill_d, {"opacity": .14}), (kill_d + .3, {"opacity": 0}),
                       (XFM, {"opacity": 0}), (XF1, {"opacity": .14})])
    sx = lambda o, s: {"opacity": o, "transform": f"scaleX({s:g})"}
    kf(f"c2bar-{r}", [(0, sx(1, .4)), (kill_d, sx(1, .46)), (kill_d + .3, sx(0, .46)), (XFM, sx(0, .4)), (XF1, sx(1, .4))])
    ring(f"c2ring-{r}", [c2s_d])
    vis(f"kill-{r}", [(kill_d, None)])
    swap(f"c2cap-{r}-", [(0, 0), (kill_d, 1), (XF0, 0, XF)], 2)

    # subscription: A drops it with the connection; B keeps it (Redis never hears from the lost host)
    if row == "A":
        vis(f"sub-{r}", [(None, kill_d)])
        kf(f"chan-{r}", [(0, {"opacity": 1}), (kill_d, {"opacity": 1}), (kill_d + .3, {"opacity": .3}),
                         (XF0, {"opacity": .3}), (XF1, {"opacity": 1})])
        els += [text(f"stx-{r}-0", 240, 58, "st", "C2 subscribed"), text(f"stx-{r}-1", 240, 58, "why start", "subscription gone")]
        swap(f"stx-{r}-", [(0, 0), (kill_d, 1), (XF0, 0, XF)], 2)
    else:
        vis(f"sub-{r}", [(None, None)])
        vis(f"chan-{r}", [(None, None)])
        first_check = M(f["sweeps"][1][0])
        els += [text(f"stx-{r}-0", 240, 58, "st", "C2 subscribed"),
                f'<g id="stx-{r}-1" class="a"><text class="why start" x="240" y="51">socket still open:</text>'
                f'<text class="why start" x="240" y="65">wait for heartbeat</text></g>']
        swap(f"stx-{r}-", [(0, 0), (first_check, 1), (XF0, 0, XF)], 2)

    # heartbeat: renewed by C2's own sweep at 0, then runs out 30 s later unless a reap drops it first
    knots = sorted({t for t, _ in M.k if 0 <= t <= f["reap"]} | {f["reap"]} | {min(f["hb_out"], f["reap"])})
    left = lambda s: max(0.0, (f["hb_out"] - s) / (HBT / 1000))
    pts = [(0, sx(1, left(0))), (c2s_d, sx(1, left(0)))] + [(M(s), sx(1, left(s))) for s in knots if s > 0]
    pts += [(reap_d + .3, sx(0, left(f["reap"]))), (XFM, sx(0, 1)), (XF1, sx(1, 1))]
    kf(f"hb-{r}", pts)
    hbc = ["renewed by C2's own sweeps", f"C2 counts as dead at {f['hb_out']:g} s"]
    if row == "A":
        hbc.append("dropped at the reap")
        hch = [(0, 0), (kill_d, 1), (reap_d, 2), (XF0, 0, XF)]
    else:
        hbc += [f"ran out at {f['hb_out']:g} s", "dropped at the reap"]
        hch = [(0, 0), (kill_d, 1), (M(f["hb_out"]), 2), (reap_d + .6, 3), (XF0, 0, XF)]
    for i, s in enumerate(hbc):
        els.append(text(f"hbc-{r}-{i}", 214, 111, "why start" if s.startswith("ran out") else "m start", s))
    swap(f"hbc-{r}-", hch, len(hbc))

    # presence mark: A sets it at the first clock-moving sweep with no subscriber, dead 2.5 s later
    mkc = ["0 = seen subscribed"]
    if row == "A":
        mark_d = M(f["mark"])
        mkc += [f"gone since {f['mark']:g} s", f"gone ≥ {GRACE_S} s: C2 counts as dead", "dropped at the reap"]
        mch = [(0, 0), (mark_d, 1), (reap_d, 2), (reap_d + 1.6, 3), (XF0, 0, XF)]
        kf(f"gr-{r}", [(0, sx(0, 1)), (mark_d - .01, sx(0, 1)), (mark_d, sx(1, 1)), (reap_d, sx(1, 0)),
                       (reap_d + .3, sx(0, 0))])
    else:
        mkc += ["dropped at the reap"]
        mch = [(0, 0), (reap_d + .6, 1), (XF0, 0, XF)]
    for i, s in enumerate(mkc):
        els.append(text(f"mkc-{r}-{i}", 214, 163, "why start" if "dead" in s else "m start", s))
    swap(f"mkc-{r}-", mch, len(mkc))

    # narration: what each sweep that matters did
    skip, chk = f["sweeps"][0], f["sweeps"][1]
    assert not skip[1] and chk[1]
    nar = ["", "C2's own sweep at 0 s: renews its heartbeat, moves the shared clock",
           f"C1's sweep at {skip[0]:g} s: clock moved {skip[0] - f['c2_last']:g} s ago (< .5 s), so no presence check"]
    if row == "A":
        nar += [f"C1's sweep at {chk[0]:g} s moves the clock: no subscriber, so mark = {f['mark']:g}",
                f"C1's sweep at {f['reap']:g} s: gone ≥ {GRACE_S} s, so C2 is dead: J1, J2 to the list front"]
    else:
        nar += [f"C1's sweep at {chk[0]:g} s: C2's subscriber still counted, so mark stays 0",
                f"C1's sweep at {f['reap']:g} s: heartbeat ran out, so C2 is dead: J1, J2 to the list front"]
    nar += [f"wake → C1 pops {v1}, then {v2}"]
    for i, s in enumerate(nar):
        assert 400 + len(s) * 6.7 <= 936, ("narration too long", s)
        els.append(text(f"nar-{r}-{i}", 400, 10, "note start", s))
    els.append(text(f"nar-{r}-{len(nar)}", 936, 10, "st end", f"J1, J2 back in the list at {f['reap']:g} s"))
    nch = [(0, 0), (c2s_d, 1), (M(skip[0]), 2), (M(chk[0]), 3)]
    if row == "B":
        nch.append((M(chk[0]) + 2.4, 0))
    nch += [(reap_d, 4), (wake_d, 5), (max(ack_d.values()) + .2, 6), (XF0, 0, XF)]
    swap(f"nar-{r}-", nch, len(nar) + 1)

    # sweeps of C1
    times = [d for d, *_ in sweeps]
    assert all(b - a >= .6 for a, b in zip(times, times[1:])), "every drawn sweep pulse is its own (ring gap)"
    ring(f"c1ring-{r}", times)

    # the jobs: pending (C2) → list front → pending (C1) → acked
    pos = {}
    at_ = lambda v, t, p, o: pos.setdefault(v, []).append(at(t, *p, o))
    for i, v in enumerate(("J1", "J2")):                    # start: pending under C2, in pop order
        at_(v, 0, P[i], 1)
        at_(v, reap_d, P[i], 1)
    for i, v in enumerate(f["order"]):                      # requeue: right along the row, then up into the list
        at_(v, reap_d + .3, (SLOT[i][0], P[0][1]), 1)
        at_(v, reap_d + .5, SLOT[i], 1)
    for k, (pd, v) in enumerate(((p1, v1), (p2, v2))):      # each pop takes the head, down, left into pending
        at_(v, pd, SLOT[0], 1)
        at_(v, pd + .2, (SLOT[0][0], P[k][1]), 1)
        at_(v, pd + .2 + (SLOT[0][0] - P[k][0]) / 600, P[k], 1)
        at_(v, ack_d[v], P[k], 1)
        at_(v, ack_d[v] + .3, P[k], 0)
        if k == 0:                                          # the rest moves up to the head
            at_(v2, pd + .25, SLOT[1], 1)
            at_(v2, pd + .45, SLOT[0], 1)
            assert p2 >= pd + .45
    for i, v in enumerate(("J1", "J2")):
        at_(v, XFM, P[i], 0)
        at_(v, XF1, P[i], 1)
        kf(f"{v.lower()}-{r}", pos[v])
    vis(f"own2a-{r}", [(None, reap_d)])
    vis(f"own2b-{r}", [(None, reap_d)])
    arrive = {}
    for k, (pd, v), g in ((0, (p1, v1), "g1"), (1, (p2, v2), "g2")):
        arrive[v] = walk(f"{g}-{r}", pd, POP_EDGE)
        vis(f"own1{'ab'[k]}-{r}", [(pd + .2 + (SLOT[0][0] - P[k][0]) / 600, ack_d[v])])
    shown = (max(ack_d.values()) + .3, min(max(ack_d.values()) + 2.0, XF0 - .3))
    assert shown[1] - shown[0] >= .9, ("the ack label stays long enough to read", shown)
    vis(f"acked-{r}", [shown])
    walk(f"wk-{r}", wake_d, WAKE_EDGE)
    busy = (min(arrive.values()), max(ack_d.values()))
    flash(f"c1glow-{r}", [busy], .14)
    bar(f"c1bar-{r}", [busy])
    return "\n    ".join(els)


DESC = ("Two rows show the same start on one clock, in real seconds (times not to scale). Consumer C2 holds two "
        "jobs, J1 and J2, that it dequeued and has not acknowledged: they sit in the pending hash under its owner "
        "ID. Its own sweep, once a second, has just renewed its heartbeat to now plus {hbt} seconds and its "
        "presence mark is 0, meaning a sweep has seen it subscribed to its wake channel. Consumer C1 is alive, "
        "idle, and sweeps once a second. At {kill} s C2 stops without calling Close, so it sweeps no more. "
        "A sweep moves the shared clock only if half a second has passed since the last move, and only a sweep "
        "that moves it checks subscribers: C1's sweep at {skip} s comes {skip} s after C2's last sweep moved the "
        "clock, so it checks heartbeats but not subscribers. "
        "Row A, process killed: Redis drops the dead process's subscription at once. C1's sweep at {mark} s moves "
        "the clock, finds no subscriber and sets C2's presence mark to {mark}. At {reapA} s a sweep finds C2 gone "
        "for at least {grace} seconds and takes it for dead: J1 and J2 go back to the front of the list in one step (J2 at "
        "the head), and C2's heartbeat and mark are dropped although the heartbeat had time left. "
        "Row B, host lost: Redis keeps the lost host's socket open, so it still counts C2's subscription and the "
        "mark stays 0. C2's heartbeat runs out at {hbt} s and C1's sweep at {reapB} s takes C2 for dead, with the "
        "same requeue. In both rows the requeue wakes C1, which pops J2 and then J1, runs them and acknowledges "
        "them, leaving the list and the pending hash empty. Row A got its jobs back after about {reapA} seconds, "
        "row B after about {reapB}. The picture then fades back to the start.")


def build():
    global XF0, XFM, XF1
    kfu.rules.clear()
    kfu.elements.clear()
    sims = {row: simulate(row) for row in ROWS}
    fs = {row: facts(row, *sims[row]) for row in ROWS}
    fa, fb = fs["A"], fs["B"]
    assert fa["reap"] < fb["reap"], "row A requeues before row B"
    assert all(FF[0] - .5 >= t for t, _ in fa["acks"]) and fb["hb_out"] - 1 >= FF[1], "fast-forward skips nothing"
    for f in fs.values():                                    # in it, only sweeps that find nothing to do
        assert all(tk and notes == [] for t, tk, notes in f["sweeps"] if FF[0] < t < FF[1]), f["sweeps"]
    M = build_map(fa, fb)
    XF0 = M(END / 1000)
    XF1 = XF0 + XF
    XFM = (XF0 + XF1) / 2
    kfu.D = float(math.ceil(XF1 + .5))                      # whole seconds, a calm start state after

    tpl = open("dead.tpl.svg").read()
    head, rest = tpl.split("<!--ROW-->")
    block, tail = rest.split("<!--/ROW-->")
    rows = []
    for row, o in ROWS.items():
        b = block
        for k, v in (("{r}", o["r"]), ("{Y}", str(o["Y"])), ("{TITLE}", esc(o["title"])), ("{KILL}", o["kill"])):
            b = b.replace(k, v)
        rows.append(b.replace("<!--ROW-ELEMENTS-->", draw_row(row, fs[row], M)))

    # clock: real seconds, rounded; fast-forward and replay notes
    n = int(END / 1000) + 1                                 # whole seconds, as a stopwatch shows them
    clock = [text(f"clk{i}", 936, 22, "clk", f"t = {i} s") for i in range(n)]
    swap("clk", [(0, 0)] + [(M(i), i, .08) for i in range(1, n)] + [(XF0, 0, XF)], n)
    vis("ff", [(M(FF[0]), M(FF[1]))])
    vis("replay", [(XF0 - .3, XF1 + .3)])

    desc = DESC.format(hbt=HBT // 1000, kill=f"{fa['kill']:g}", skip=f"{fa['skip']:g}", mark=f"{fa['mark']:g}",
                       reapA=f"{fa['reap']:g}", reapB=f"{fb['reap']:g}", grace=GRACE_S)
    sep = ROWS["B"]["Y"] - 8
    svg = (head + "".join(rows) + tail).replace("<!--CLOCK-->", "\n  ".join(clock))
    for k, v in (("{H}", str(H)), ("{H1}", str(H - 1)), ("{SEP}", str(sep)), ("{DESC}", esc(desc))):
        svg = svg.replace(k, v)
    still = round(M(11.5), 2)                                # row A requeued and acked, row B's heartbeat still running;
                                                             # before the fast-forward, mid-second, clear of the clock's fade

    # seam: every track ends where it starts (the cross-fade puts the start state back), all inside D
    for name, pts in kfu.rules:
        assert pts[-1][0] <= kfu.D, (name, pts[-1][0], kfu.D)
        a, b = kfu.padded(name, pts)[0][1], kfu.padded(name, pts)[-1][1]
        assert a == b or a.get("opacity") == b.get("opacity") == 0, (name, a, b)   # hidden: place doesn't matter
    kfu.render(svg, "../redisq-dead.svg", still=still)
    return fs, M, still


if __name__ == "__main__":
    fs, M, still = build()
    for row, f in fs.items():
        print(f"row {row}: {ROWS[row]['title']}")
        ev = [(f["c2_last"], "C2's own sweep: heartbeat runs out at %g, mark 0, moves the clock" % f["hb_out"]),
              (f["kill"], "C2 stops" + (": Redis drops its subscription" if row == "A" else ": its socket stays open"))]
        for t, tk, notes in f["sweeps"]:
            if FF[0] < t < FF[1] and all(n[0] == "waiting" for n in notes) and tk:
                continue
            ev.append((t, "C1 sweep, %s%s" % ("moves the clock" if tk else "clock not moved: no presence check",
                                               (": " + ", ".join(" ".join(map(str, n)) for n in notes)) if notes else "")))
            if t >= f["acks"][-1][0] + 1 and row == "A" and t < FF[0]:
                break
        ev.append((f["wake"], f"wake(2): woken {f['woken']}"))
        ev += [(t, f"C1 pops {v}") for t, v in f["pops"]] + [(t, f"C1 acks {v}") for t, v in f["acks"]]
        for t, s in sorted(ev):
            if row == "B" and FF[0] < t < FF[1]:
                continue
            print(f"  real {t:6.3f}  drawn {M(t):6.2f}  {s}")
    print("fast-forward real", FF, "drawn", round(M(FF[0]), 2), "→", round(M(FF[1]), 2))
    print("cross-fade", round(XF0, 2), "→", round(XF1, 2), " D", kfu.D, " still", still)
