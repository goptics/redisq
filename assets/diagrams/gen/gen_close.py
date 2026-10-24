"""redisq-close.svg: consumer C2 closes with three jobs running and unacknowledged; Close hands them back to the
front of the list at once and wakes C1, the only idle consumer, which runs them again.

The timeline is simulated: the enqueue burst and its wakes, varmq's pull loop, the once-a-second nudges, C1's
Subscribe, C2's Close (leave, release script, wake) and C1's acks. Facts and invariants: STORYBOARD.md, section 3.
"""
import heapq
import math

import kfu
from kfu import at, bar, flash, kf, states

# ---- scenario, in drawn seconds (times not to scale) ----
CONC = 3                  # worker slots per consumer
POP = 1                   # items per pop call. varmq v1.9.0 pulls an ack source one DequeueWithAckId at a time
                          # (varmq/source.go:125-136: maxBatch() == 1, Next → DequeueWithAckId; worker_eventloop.go:26,
                          # 185), so a wake pops once per free slot, back to back. varmq main after v1.9.0
                          # (worker_credit.go, 56a4e93) pops every free slot with one DequeueBatchWithAckIds: POP = CONC.
ENQ = [("J1", .40), ("J2", .46), ("J3", .52)]   # Enqueue calls, back to back
ENQ_FLY = .5              # producer → list; the RPUSH + wake script runs as the job lands (distributed.go:9-11)
WORK = {"J1": 2.6, "J2": 2.9, "J3": 3.2}         # run time of each job, wherever it runs
C2_TICK0 = .8             # C2 nudges at .8 + k, its phase in diagram 2
C1_START = 2.4            # C1 is built (its nudge ticker starts) and subscribes
CLOSE = 3.4               # C2.Close()
LEAVE = .2                # leave(): SREM, drawn
REL = .32                 # release script, drawn
EXIT_AFTER = .3           # C2's process exits this long after Close's wake
POP_GAP = .05             # back-to-back pops of one pull loop, drawn
POP_MOVE = .6             # list → pending row, drawn
CHIP_IN, CHIP_OUT = .25, .15                     # a chip slides into / out of :idle, drawn
STEPS_FOR = 1.3           # Close's step list stays this long after its last step (the wake)
VT = 16.0                 # visibility timeout as drawn (default 5 min, queue.go:65)
HOLD, XFADE = 1.0, .5     # end state held, then cross-fade to the start state
DOT_V, WAKE_V = 520, 520  # px/s
HORIZON = 30

# ---- geometry (template coordinates) ----
LIST_Y, SLOT_X = 150, [420, 384, 348]            # list slots, head first (top-left of a job square)
PROD = (64, 150)
ROW_X, ROW_Y = 212, [266, 314, 362]              # pending rows (top-left of a job square)
CY = {"C1": 96, "C2": 284}                       # consumer box tops
POP_PATH = {"C1": [(470, 165), (740, 165)], "C2": [(470, 165), (500, 165), (500, 370), (740, 370)]}
WAKE_PATH = {"C1": [(690, 248), (716, 248), (716, 180), (740, 180)],
             "C2": [(690, 270), (716, 270), (716, 296), (740, 296)]}
STATES = {"C1": ["not started", "idle", "1/3 busy", "2/3 busy", "3/3 busy"],
          "C2": ["idle", "1/3 busy", "2/3 busy", "3/3 busy", "Close()", "gone"]}


def plen(pts):
    return sum(abs(x1 - x0) + abs(y1 - y0) for (x0, y0), (x1, y1) in zip(pts, pts[1:]))


def travel(pts, v):
    return .05 + plen(pts) / v


def walk(name, t, pts, v):
    """Dot along pts at v px/s, visible on the edge, hidden at the end (inside a box). Returns arrival time."""
    out, (x, y) = [at(t, *pts[0], 0), at(t + .05, *pts[0], 1)], pts[0]
    t += .05
    for nx, ny in pts[1:]:
        t += (abs(nx - x) + abs(ny - y)) / v
        x, y = nx, ny
        out.append(at(t, x, y, 1))
    out.append(at(t + .05, x, y, 0))
    kf(name, out)
    return t


class Sim:
    """Redis state (list, pending, :idle) and two consumers, driven by a heapq of events."""

    def __init__(self):
        self.ev, self.n = [], 0
        self.lst, self.x = [], {}                    # list head first; drawn x of each listed job
        self.pending = {}                            # job -> {owner, row, t, seq}
        self.idle = {"C2"}                           # Subscribe put C2 there before the loop starts
        self.idle_log = {"C1": [], "C2": []}         # (t, member?, why)
        self.c = {m: dict(alive=False, consumer=False, sub=False, closed=False, busy=0, slots=[None] * CONC,
                          pulling=False, signal=False, seq=0, beat=False, ticks=[], free=False) for m in ("C1", "C2")}
        self.c["C2"].update(alive=True, consumer=True, sub=True, free=True)   # free: Notification.idle
        self.log = []
        self.items = {j: [] for j, _ in ENQ}         # job square keyframes
        self.where = {j: [] for j, _ in ENQ}         # (t, place) per job, for the invariants
        self.entries = []                            # pending lifetimes {owner, row, t, end, how}
        self.runs = []                               # {c, slot, job, reserved, start, end, frac}
        self.pops, self.acks, self.failed_acks, self.wakes, self.push_wakes = [], [], [], [], []
        self.busy_log = {"C1": [], "C2": []}
        self.calls, self.mark = [], {}

    # -- plumbing --
    def at_(self, t, kind, *a):
        self.n += 1
        heapq.heappush(self.ev, (round(t, 4), self.n, kind, a))

    def say(self, t, msg):
        self.log.append((round(t, 2), msg))

    def run(self):
        while self.ev:
            t, _, kind, a = heapq.heappop(self.ev)
            if t > HORIZON:
                break
            getattr(self, "on_" + kind)(t, *a)

    def sadd(self, t, m, why):
        if m in self.idle:
            return False
        self.idle.add(m)
        self.idle_log[m].append((t, True, why))
        self.say(t, f"SADD :idle {m} ({why}) -> :idle = {sorted(self.idle)}")
        return True

    def srem(self, t, m, why):
        if m not in self.idle:
            return False
        self.idle.discard(m)
        self.idle_log[m].append((t, False, why))
        self.say(t, f"SREM :idle {m} ({why}) -> :idle = {sorted(self.idle)}")
        return True

    def length(self, t, m):
        """Len() of a DistributedQueue: lenIdle, which SADDs a consumer that reads 0 (notification.go:45-47)."""
        n = len(self.lst)
        if n == 0 and self.c[m]["consumer"]:
            self.sadd(t, m, "Len() = 0")
            self.c[m]["free"] = True                 # notification.go:184-186
        return n

    def set_busy(self, t, m, d):
        st = self.c[m]
        st["busy"] += d
        assert 0 <= st["busy"] <= CONC, (m, t, st["busy"])                       # capacity is never exceeded
        self.busy_log[m].append((t, st["busy"]))

    # -- Redis scripts --
    def wake(self, t, count, why):
        """wake(count): SPOP a member, PUBLISH to its channel, until count woken or :idle is empty (notification.go:52-64)."""
        woken, spopped = 0, []
        while woken < count and self.idle:
            assert len(self.idle) == 1, ("SPOP is random: the drawing relies on one member", t, self.idle)
            m = self.idle.pop()
            self.idle_log[m].append((t, False, "SPOP"))
            spopped.append(m)
            if self.c[m]["sub"]:                     # PUBLISH reached a subscriber
                woken += 1
                self.wakes.append((t, m))
                self.at_(t + travel(WAKE_PATH[m], WAKE_V), "wake_arrive", m)
        self.say(t, f"{why}: wake({count}) -> SPOP {spopped or 'nothing (:idle empty)'}, woken {woken}")
        return woken, spopped

    def pop_call(self, t, m, want, why):
        """DequeueWithAckId (POP == 1) or DequeueBatchWithAckIds: list head → pending + deadline + beat, one script."""
        st = self.c[m]
        got = self.lst[:want]
        del self.lst[:want]
        assert got, (t, m, "a pop right after Len() > 0 finds an item")
        for job in got:
            st["seq"] += 1
            used = {p["row"] for p in self.pending.values()}
            row = max(r for r in range(3) if r not in used)                       # drawn row: lowest free
            self.pending[job] = dict(owner=m, row=row, t=t, seq=st["seq"])
            st["beat"] = True                                                    # queue.go:541-543
            slot = st["slots"].index(None)
            st["slots"][slot] = job
            self.set_busy(t, m, +1)
            x = self.x.pop(job)
            dy, dx = ROW_Y[row] - LIST_Y, x - ROW_X                              # down, then left
            self.items[job] += [at(t, x, LIST_Y, 1), at(t + POP_MOVE * dy / (dx + dy), x, ROW_Y[row], 1),
                                at(t + POP_MOVE, ROW_X, ROW_Y[row], 1)]
            self.where[job].append((t, "pending:" + m))
            arrive = t + travel(POP_PATH[m], DOT_V)
            self.pops.append((t, m, job, why))
            self.runs.append(dict(c=m, slot=slot, job=job, reserved=t, start=arrive, end=None, frac=None))
            self.at_(arrive + WORK[job], "job_end", m, slot, job)
        if POP > 1 and not self.lst and len(got) < want and st["consumer"]:     # batch idle rule (queue.go popHead)
            self.sadd(t, m, "short pop")
        st["free"] = POP > 1 and not self.lst and len(got) < want                # distributed.go:72, 82
        self.say(t, f"{m} pops {got} ({why}) -> pending {self.owners()}, list {self.lst}")

    def owners(self):
        return {j: p["owner"] for j, p in sorted(self.pending.items())}

    # -- varmq v1.9.0 pull loop: `for active && busy < conc && pending() > 0 { processNextJob }` --
    def signal(self, t, m, why):
        st = self.c[m]
        if not st["alive"]:
            return
        if st["pulling"]:
            st["signal"] = True                      # 1-slot channel: runs once the current loop ends
            return
        if st["busy"] >= CONC:
            self.say(t, f"{m} ignores the {why}: {CONC}/{CONC} busy")
        self.loop(t, m, why)

    def loop(self, t, m, why):
        st = self.c[m]
        if st["alive"] and st["busy"] < CONC and self.length(t, m) > 0:
            st["pulling"] = True
            self.pop_call(t, m, min(CONC - st["busy"], POP), why)
            self.at_(t + POP_GAP, "loop", m, why)
            return
        st["pulling"] = False
        if st["signal"]:
            st["signal"] = False
            self.loop(t, m, "queued signal")

    # -- events --
    def on_loop(self, t, m, why):
        self.loop(t, m, why)

    def on_enqueue(self, t, job):
        self.calls.append(t)
        self.at_(t + ENQ_FLY, "push", job, t)

    def on_push(self, t, job, t_call):
        self.lst.append(job)                                                     # RPUSH, tail
        x = SLOT_X[len(self.lst) - 1]
        self.x[job] = x
        self.items[job] += [at(t_call, *PROD, 0), at(t_call + .08, PROD[0] + 12, PROD[1], 1), at(t, x, LIST_Y, 1)]
        self.where[job].append((t, "list"))
        n = len(self.lst)
        woken, _ = self.wake(t, n, f"Enqueue {job}: RPUSH, n = {n}")
        self.push_wakes.append((job, n, woken))

    def on_wake_arrive(self, t, m):
        self.say(t, f"{m} receives the wake")
        self.signal(t, m, "wake")

    def on_nudge(self, t, m):
        st = self.c[m]
        if not st["consumer"]:
            return                                   # leave() / Stop: the nudge loop skips, then returns
        st["ticks"].append(t)
        if not self.lst:
            if st["free"]:                           # only a consumer whose own last look found no work
                self.sadd(t, m, "nudge, queue empty")
            elif m not in self.idle:
                self.mark.setdefault(f"nudge-stays-{m}", (t, st["busy"]))
                self.say(t, f"{m} nudge: queue empty, {st['busy']}/{CONC} busy -> stays out of :idle")
        else:
            self.srem(t, m, "nudge, items waiting")
            self.say(t, f"{m} nudge: {len(self.lst)} waiting -> poke")
            self.signal(t, m, "nudge poke")
        if t + 1 <= HORIZON:
            self.at_(t + 1, "nudge", m)

    def on_subscribe(self, t, m):
        self.c[m].update(alive=True, consumer=True, sub=True, free=True)
        self.say(t, f"{m} starts and subscribes")
        self.sadd(t, m, "Subscribe")
        self.at_(t + 1, "nudge", m)                  # the ticker started when the queue was built

    def on_close(self, t, m):
        st = self.c[m]
        st["closed"] = True                          # Queue.Close cancels its context: Acknowledge fails from now
        st["consumer"] = False                       # leave()
        self.mark["close"] = t
        self.mark["idle_at_close"] = set(self.idle)
        self.mark["left"] = self.srem(t, m, "leave()")
        self.say(t, f"{m}.Close() with {[r['job'] for r in self.runs if r['c'] == m and r['end'] is None]} running")
        self.at_(t + LEAVE + .02, "release", m)

    def on_release(self, t, m):
        ids = sorted((p["seq"], j) for j, p in self.pending.items() if p["owner"] == m)   # table.sort: pop order
        for _, j in ids:
            self.lst.insert(0, j)                                                # LPUSH each (queue.go:747)
        for _, j in ids:
            p = self.pending.pop(j)
            x = SLOT_X[self.lst.index(j)]
            self.x[j] = x
            dx, dy = x - ROW_X, ROW_Y[p["row"]] - LIST_Y                         # right, then up
            self.items[j] += [at(t, ROW_X, ROW_Y[p["row"]], 1), at(t + REL * dx / (dx + dy), x, ROW_Y[p["row"]], 1),
                              at(t + REL, x, LIST_Y, 1)]
            self.where[j].append((t, "list"))
            self.entries.append(dict(owner=m, row=p["row"], t=p["t"], end=t, how="release"))
        self.c[m]["beat"] = False                    # ZREM heartbeat + presence (queue.go:840-849)
        self.mark["release"] = (t, [j for _, j in ids], list(self.lst))
        self.say(t, f"{m} release script: LPUSH {[j for _, j in ids]} -> list {self.lst}; heartbeat, presence dropped")
        self.at_(t + REL, "release_wake", m, len(ids))

    def on_release_wake(self, t, m, n):
        self.mark["idle_at_wake"] = set(self.idle)
        woken, spopped = self.wake(t, n, f"{m}.Close requeued {n}")
        self.mark["close_wake"] = (t, woken, spopped)
        self.c[m]["sub"] = False                     # Notification.Stop closes the subscription
        self.at_(t + EXIT_AFTER, "exit", m)

    def on_exit(self, t, m):
        self.c[m]["alive"] = False
        self.mark["exit"] = t
        for r in self.runs:
            if r["c"] == m and r["end"] is None:
                assert r["start"] < t < r["start"] + WORK[r["job"]], ("cut off mid-run", r, t)
                r["end"], r["frac"] = t, (t - r["start"]) / WORK[r["job"]]
        self.say(t, f"{m} exits: runs cut off, no ack sent")

    def on_job_end(self, t, m, slot, job):
        st = self.c[m]
        if not st["alive"]:
            return                                   # cut off when the process exited
        r = next(r for r in self.runs if r["c"] == m and r["slot"] == slot and r["job"] == job and r["end"] is None)
        r["end"] = t
        if st["closed"]:
            self.failed_acks.append((t, m, job))     # Acknowledge after Close fails (queue.go:337-339)
        else:
            p = self.pending.pop(job)                # Acknowledge: HDEL + ZREM (queue.go:335-355)
            assert p["owner"] == m, (job, p, m)
            self.acks.append((t, m, job))
            self.items[job] += [at(t, ROW_X, ROW_Y[p["row"]], 1), at(t + .3, ROW_X, ROW_Y[p["row"]], 0)]
            self.where[job].append((t, "done"))
            self.entries.append(dict(owner=m, row=p["row"], t=p["t"], end=t, how="ack"))
            self.say(t, f"{m} acks {job} -> pending {self.owners()}")
        st["slots"][slot] = None
        self.set_busy(t, m, -1)
        self.length(t, m)                            # varmq: freePoolNode reads pending() (worker_pool.go:166)
        self.signal(t, m, "job end")                 # then notifyToPullNextJobs


def collapse(changes, gap=.15):
    """Drop states shown for less than gap (pops .06 s apart): keep the last of a quick run."""
    out = []
    for t, i in sorted(changes):
        while out and t - out[-1][0] < gap and out[-1][0] > 0:
            out.pop()
        if not out or out[-1][1] != i:
            out.append((t, i))
    return out


def chip(m, log, start_in, xf0):
    pts = [(0, {"opacity": 1 if start_in else 0, "transform": kfu.tr(0 if start_in else 16, 0)})]
    for t, member, _ in log:
        if member:
            pts += [(t, {"opacity": 0, "transform": kfu.tr(16, 0)}), (t + CHIP_IN, {"opacity": 1, "transform": kfu.tr(0, 0)})]
        else:
            pts += [(t, {"opacity": 1, "transform": kfu.tr(0, 0)}), (t + CHIP_OUT, {"opacity": 0, "transform": kfu.tr(16, 0)})]
    end_in = log[-1][1] if log else start_in
    if end_in != start_in:                         # cross-fade back to the start state
        pts += [(xf0, pts[-1][1]), (xf0 + XFADE, pts[0][1])]
    kf(f"chip-{m}", pts)


def pulse(name, times):
    pts = [(0, {"opacity": 0, "transform": "scale(1)"})]
    for t in times:
        pts += [(t, {"opacity": 0, "transform": "scale(1)"}), (t + .1, {"opacity": .9, "transform": "scale(1.3)"}),
                (t + .6, {"opacity": 0, "transform": "scale(2.4)"})]
    kf(name, pts)


def consumer_svg(m, used_labels):
    y0 = CY[m]
    g = [f'  <g id="c-{m}" class="a">',
         f'    <rect class="box wk" x="740" y="{y0}" width="196" height="96" rx="8"/>',
         f'    <rect id="glow-{m}" class="a glow" x="740" y="{y0}" width="196" height="96" rx="8"/>',
         f'    <text class="t" x="754" y="{y0 + 17}" style="text-anchor:start">Consumer {m}</text>']
    for k, name in enumerate(STATES[m]):
        cls = "a m st st-on" if name in ("Close()",) else "a m st"
        g.append(f'    <text id="st-{m}-{k}" class="{cls}" x="922" y="{y0 + 17}">{name}</text>')
    g.append(f'    <text class="m" x="754" y="{y0 + 34}" style="text-anchor:start">DistributedQueue · 3 slots</text>')
    for s in range(CONC):
        y = y0 + 51 + 16 * s
        g.append(f'    <rect class="track" x="786" y="{y - 2}" width="136" height="4" rx="2"/>')
        g.append(f'    <rect id="bar-{m}-{s}" class="a lin bar" x="786" y="{y - 2}" width="136" height="4" rx="2"/>')
    for s, job in used_labels:
        g.append(f'    <text id="jl-{m}-{s}-{job}" class="a m" x="768" y="{y0 + 51 + 16 * s}">{job}</text>')
    g += [f'    <circle class="pip" cx="920" cy="{y0}" r="4"/>',
          f'    <circle id="ring-{m}" class="a pring" cx="920" cy="{y0}" r="5"/>', '  </g>']
    return "\n".join(g)


def rows_svg(tags):
    out = []
    for r, y in enumerate(ROW_Y):
        c = y + 15
        out.append(f'  <rect class="track" x="294" y="{c - 3}" width="160" height="6" rx="3"/>')
        out.append(f'  <rect id="dl-r{r}" class="a lin bar" x="294" y="{c - 3}" width="160" height="6" rx="3"/>')
        out.append(f'  <text id="ok-r{r}" class="a ok" x="374" y="{c + 13}">✓ acked by C1</text>')
    for r, m in sorted(tags):
        c = ROW_Y[r] + 15
        out.append(f'  <g id="tag-r{r}-{m}" class="a"><rect class="chip" x="248" y="{c - 9}" width="36" height="18" rx="9"/>'
                   f'<text class="ttxt" x="266" y="{c}">{m}</text></g>')
    return "\n".join(out)


def build():
    kfu.rules.clear()
    kfu.elements.clear()
    s = Sim()
    for j, t in ENQ:
        s.at_(t, "enqueue", j)
    s.at_(C2_TICK0, "nudge", "C2")
    s.at_(C1_START, "subscribe", "C1")
    s.at_(CLOSE, "close", "C2")
    s.run()

    end = max(t for t, _, _ in s.acks) + .3                                    # last pending entry gone
    xf0 = end + HOLD
    xf1 = xf0 + XFADE
    kfu.D = float(math.ceil(xf1 + .5))                                          # whole seconds: 1 s nudge pulses

    # ---------------- invariants (STORYBOARD.md section 3) ----------------
    close, (rel_t, rel_ids, rel_list) = s.mark["close"], s.mark["release"]
    wake_t, woken, spopped = s.mark["close_wake"]
    # only J1's push wakes: n >= 1 every time, but :idle is empty for J2, J3 (distributed.go:9-11, notification.go:58-59)
    assert s.push_wakes == [("J1", 1, 1), ("J2", 2, 0), ("J3", 3, 0)], s.push_wakes
    # a woken consumer pops while it has a free slot and Len() > 0: C2's one wake → J1-J3, one pop call per POP items.
    # varmq v1.9.0: ack sources never batch (source.go:127 maxBatch() == 1, source.go:136 Next → DequeueWithAckId;
    # each hand-off is capped at m.maxBatch(), worker_eventloop.go:185; the loop repeats while busy < concurrency and
    # pending > 0, worker_eventloop.go:26), so one wake makes 3 back-to-back pops, not one batch pop.
    c2_pops = [(t, j, w) for t, m, j, w in s.pops if m == "C2"]
    assert [j for _, j, _ in c2_pops] == ["J1", "J2", "J3"] and {w for *_, w in c2_pops} == {"wake"}, c2_pops
    assert len({t for t, _, _ in c2_pops}) == math.ceil(CONC / POP), c2_pops
    landed = {j: s.where[j][0][0] for j, _ in ENQ}                              # RPUSH instants
    assert s.wakes[0][1] == "C2" and s.wakes[0][0] + travel(WAKE_PATH["C2"], WAKE_V) > max(landed.values()), \
        "the whole burst is in the list before C2's wake arrives"
    # full C2's nudge leaves it out of :idle (notification.go:74-77), so it is not there at Close; leave() runs
    # before the release all the same (distributed.go:90-91, notification.go:218-221)
    t_stay, busy_stay = s.mark["nudge-stays-C2"]
    assert busy_stay == CONC and t_stay < close, (t_stay, busy_stay)
    assert "C2" not in s.mark["idle_at_close"] and not s.mark["left"] and close < rel_t
    assert not [t for t, member, _ in s.idle_log["C2"] if member], "C2 never rejoins :idle while it holds jobs"
    assert all(t < close for t in s.c["C2"]["ticks"]), "no C2 nudge after Close"
    # the release: all of C2's entries in one script, LPUSH in pop order → J3 J2 J1 from the head; beat dropped
    assert rel_ids == ["J1", "J2", "J3"] and rel_list == ["J3", "J2", "J1"], (rel_ids, rel_list)   # queue.go:840-849, 747
    assert not s.c["C2"]["beat"]
    assert [e["how"] for e in s.entries if e["owner"] == "C2"] == ["release"] * 3
    # wake(3) can only reach C1: :idle == {C1} when it runs; woken 1, the second SPOP finds nothing
    assert s.mark["idle_at_wake"] == {"C1"} and woken == 1 and spopped == ["C1"], (s.mark, woken, spopped)
    # C1 takes J3, J2, J1, triggered by that wake, not by a timer, well under a second after Close
    c1_pops = [(t, j, w) for t, m, j, w in s.pops if m == "C1"]
    assert [j for _, j, _ in c1_pops] == ["J3", "J2", "J1"] and {w for *_, w in c1_pops} == {"wake"}, c1_pops
    assert c1_pops[-1][0] - close < 1, c1_pops
    assert not [t for t in s.c["C1"]["ticks"] if rel_t <= t <= c1_pops[-1][0]], "no C1 nudge inside Close → pop"
    # every job acked once, by C1; C2's runs were cut off, never acked (at-least-once: they ran twice)
    assert sorted(j for *_, j in s.acks) == ["J1", "J2", "J3"] and {m for _, m, _ in s.acks} == {"C1"}, s.acks
    assert not s.failed_acks
    c2_runs = [r for r in s.runs if r["c"] == "C2"]
    assert len(c2_runs) == 3 and all(r["frac"] is not None and r["frac"] < 1 for r in c2_runs), c2_runs
    # no deadline is ever reached: Close hands the jobs back, not the sweep; bars stay short
    for e in s.entries:
        assert e["end"] - e["t"] < VT * .3, e
    # each job is in exactly one place, in this order
    for j, log in s.where.items():
        assert [p for _, p in log] == ["list", "pending:C2", "list", "pending:C1", "done"], (j, log)
        assert [t for t, _ in log] == sorted(t for t, _ in log), (j, log)
    # end of the run: list and pending empty, C1 idle in :idle, C2 gone; nothing changes after it
    assert not s.lst and not s.pending and s.idle == {"C1"}, (s.lst, s.pending, s.idle)
    assert s.c["C1"]["busy"] == 0 and not s.c["C2"]["alive"]
    assert all(t <= end for log in s.idle_log.values() for t, _, _ in log), s.idle_log
    assert end + 1 <= xf0 and xf1 + .5 <= kfu.D

    # ---------------- drawing ----------------
    for j, pts in s.items.items():
        kf(f"item{j}", pts)
    flash("enq-glow", [(t - .05, t + .25) for t in s.calls], .25)
    for i, (t, m) in enumerate(s.wakes):
        kfu.elements.append(f'  <circle id="wd{i}" class="a lin wdot" cx="0" cy="0" r="5"/>')
        walk(f"wd{i}", t, WAKE_PATH[m], WAKE_V)
    for i, (t, m, j, _) in enumerate(s.pops):
        kfu.elements.append(f'  <circle id="gd{i}" class="a lin dot" cx="0" cy="0" r="6"/>')
        arrive = walk(f"gd{i}", t, POP_PATH[m], DOT_V)
        assert abs(arrive - next(r["start"] for r in s.runs if r["c"] == m and r["job"] == j)) < 1e-9
    for j, _ in ENQ:
        kfu.elements.append(f'  <g id="item{j}" class="a"><rect class="item" x="0" y="0" width="30" height="30" rx="5"/>'
                            f'<text class="itxt" x="15" y="15">{j}</text></g>')

    # pending rows: owner tag, deadline bar, ack label
    tags = {}
    for e in s.entries:
        tags.setdefault((e["row"], e["owner"]), []).append((e["t"] + POP_MOVE, e["end"]))
    for (r, m), spans in tags.items():
        flash(f"tag-r{r}-{m}", spans, 1, rise=.15)
    for r in range(3):
        bar(f"dl-r{r}", [(e["t"], e["end"], (e["end"] - e["t"]) / VT) for e in s.entries if e["row"] == r])
        flash(f"ok-r{r}", [(e["end"], e["end"] + 1.2) for e in s.entries if e["row"] == r and e["how"] == "ack"], 1)

    # consumers
    labels = {"C1": [], "C2": []}
    for m in ("C1", "C2"):
        runs = [r for r in s.runs if r["c"] == m]
        for sl in range(CONC):
            bar(f"bar-{m}-{sl}", [(r["start"], r["end"], r["frac"]) if r["frac"] else (r["start"], r["end"])
                                  for r in runs if r["slot"] == sl])
        for r in runs:
            labels[m].append((r["slot"], r["job"]))
            flash(f"jl-{m}-{r['slot']}-{r['job']}", [(r["reserved"], r["end"])], 1)
        flash(f"glow-{m}", [(r["reserved"], r["end"]) for r in runs], .14)
        pulse(f"ring-{m}", [t for t in s.c[m]["ticks"] if t + .6 <= xf0])
    busy = {m: [(t, b) for t, b in s.busy_log[m] if t < s.mark["close"] or m == "C1"] for m in s.busy_log}
    xm = xf0 + XFADE / 2
    states("st-C1-", collapse([(0, 0), (C1_START, 1)] + [(t, 1 + b) for t, b in busy["C1"]] + [(xm, 0)]), 5)
    states("st-C2-", collapse([(0, 0)] + [(t, b) for t, b in busy["C2"]] + [(close, 4), (s.mark["exit"], 5), (xm, 0)]), 6)
    kf("c-C1", [(0, {"opacity": .15}), (C1_START, {"opacity": .15}), (C1_START + .3, {"opacity": 1}),
                (xf0, {"opacity": 1}), (xf1, {"opacity": .15})])
    kf("c-C2", [(0, {"opacity": 1}), (s.mark["exit"], {"opacity": 1}), (s.mark["exit"] + .5, {"opacity": .15}),
                (xf0, {"opacity": .15}), (xf1, {"opacity": 1})])
    chip("C1", s.idle_log["C1"], False, xf0)
    chip("C2", s.idle_log["C2"], True, xf0)

    # labels: why nothing happens, what each step does
    nowake = [(landed[j], landed[j] + .5) for j, _, w in s.push_wakes if w == 0]
    # Close's three steps appear one by one as they run and leave together, as one numbered list under :idle
    steps_end = wake_t + STEPS_FOR
    steps = {"note-leave": close, "note-release": rel_t, "note-wake": wake_t}
    assert close < rel_t < wake_t, steps
    notes = {"why-nowake": nowake, "note-nudge": [(t_stay, t_stay + 1.1)]}
    for k, v in notes.items():
        flash(k, v, 1)
    for k, t0 in steps.items():
        flash(k, [(t0, steps_end)], 1)
    # one label at a time in the space under :idle (y 290-360): drawn spans (fade included) never overlap
    under_idle = sorted(sp for v in list(notes.values()) + [[(close, steps_end)]] for sp in kfu.merge(v, .37))
    for (s0, e0), (s1, _) in zip(under_idle, under_idle[1:]):
        assert e0 + .25 <= s1, ("labels under :idle overlap", under_idle)
    flash("why-c2", [(rel_t, s.mark["exit"] + 1.6)], 1)
    flash("replay", [(xf0 - .1, xf1 + .2)], 1)

    # seam: every element ends as it starts (the cross-fade resets what the run changed)
    for name, pts in kfu.rules:
        p = kfu.padded(name, pts)
        hidden = p[0][1].get("opacity") == 0 == p[-1][1].get("opacity")         # invisible at both ends: equal
        assert hidden or p[0][1] == p[-1][1], ("seam", name, p[0], p[-1])

    # still: jobs back in the list, C1's chip gone from :idle (SPOP), the wake on its way to C1, all 3 steps shown
    chip_gone, dot_arrive = wake_t + CHIP_OUT, wake_t + travel(WAKE_PATH["C1"], WAKE_V)
    still = round((chip_gone + dot_arrive) / 2, 2)
    assert rel_t + REL <= still and chip_gone <= still < dot_arrive and still < steps_end, (still, chip_gone, dot_arrive)
    assert s.mark["exit"] > still, "C2 still shows Close() with its runs going in the still"
    assert not [t for t, _, _ in s.idle_log["C1"] if wake_t < t <= still], "C1's chip stays out of :idle in the still"
    tpl = open("close.tpl.svg").read().replace("{VT}", f"{VT:g}")
    if POP > 1:                                                            # the drawn pop follows the simulated one
        tpl = tpl.replace("DequeueWithAckId per free slot", "DequeueBatchWithAckIds: all free slots")
        tpl = tpl.replace("one DequeueWithAckId call per free slot", "one DequeueBatchWithAckIds call for all free slots")
        tpl = tpl.replace("right away, one call per free slot,", "right away in one call,")
    assert POP == 1 or "DequeueWithAckId " not in tpl
    tpl = tpl.replace("<!--ROWS-->", rows_svg(tags))
    tpl = tpl.replace("<!--CONSUMERS-->", consumer_svg("C1", labels["C1"]) + "\n" + consumer_svg("C2", labels["C2"]))
    kfu.render(tpl, "../redisq-close.svg", still=still)
    return s, end, xf0, xf1, still


if __name__ == "__main__":
    s, end, xf0, xf1, still = build()
    for t, msg in s.log:
        if t <= xf0:
            print(f"{t:6.2f}  {msg}")
    print(f"{end:6.2f}  end state: list {s.lst}, pending {s.owners()}, :idle {sorted(s.idle)}; hold to {xf0:.2f}, "
          f"cross-fade to {xf1:.2f}")
    print("nudges", {m: [t for t in s.c[m]["ticks"] if t < xf0] for m in ("C2", "C1")})
    print("C2 runs cut at", [(r["job"], round(r["frac"], 2)) for r in s.runs if r["c"] == "C2"],
          "acks", [(round(t, 2), m, j) for t, m, j in s.acks])
    print("D", kfu.D, "still", still)
