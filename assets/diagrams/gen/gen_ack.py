"""redisq-ack.svg: one Queue object, two jobs; A is acknowledged, B is not and comes back after its deadline.

The timeline is simulated: pops, acks, deadlines and the once-a-second sweep. Facts and invariants:
STORYBOARD.md, section 1.
"""
import kfu
from kfu import at, bar, flash, kf, ring

VT = 4.0              # visibility timeout as drawn (default 5 min, queue.go:83)
WORK = 2.0            # seconds a job takes
SWEEP_PHASE = .3      # the object's sweep ticks at k + .3 (queue.go:908-909); a pulse lasts .6 s, so all fit in D
ENQ = {"A": .4, "B": .9}
DOT_V = 420           # px/s for dots on edges

HEAD, SLOT = (420, 160), {0: 420, 1: 384, 2: 348}     # list slots, head first
ROW = {0: (212, 273), 1: (212, 321)}                  # pending rows (top-left of the job square)
PROD = (90, 160)
POP_EDGE = [(470, 175), (740, 175)]
ACK_EDGE = [(780, 210), (780, 240), (450, 240), (450, 260)]


def path_len(pts):
    return sum(abs(x1 - x0) + abs(y1 - y0) for (x0, y0), (x1, y1) in zip(pts, pts[1:]))


def walk(name, t, pts):
    """Dot along pts at DOT_V, visible on the edge, hidden at the end (inside a box). Returns arrival time."""
    out, x, y = [at(t, *pts[0], 0), at(t + .05, *pts[0], 1)], *pts[0]
    t += .05
    for nx, ny in pts[1:]:
        t += (abs(nx - x) + abs(ny - y)) / DOT_V
        x, y = nx, ny
        out.append(at(t, x, y, 1))
    out.append(at(t + .05, x, y, 0))
    kf(name, out)
    return t


def build():
    kfu.rules.clear()
    kfu.elements.clear()
    item = {k: [] for k in ENQ}                 # keyframes per job square
    where = {k: [] for k in ENQ}                # (t, place) log for the invariants
    lst = []                                    # list contents, head first

    def place(k, t, p):
        where[k].append((t, p))

    # ---- enqueue: RPUSH to the tail (queue.go Enqueue) ----
    for k, t in ENQ.items():
        lst.append(k)
        x = SLOT[len(lst) - 1]
        item[k] += [at(t, *PROD, 0), at(t + .1, PROD[0] + 12, PROD[1], 1), at(t + .7, x, HEAD[1], 1)]
        place(k, t + .7, "list")

    pops, acks, busy, deadlines, bars, rows_free = [], [], [], {}, {0: [], 1: []}, [0, 1]
    dots = iter(["dot0", "dot1", "dot2"])
    ack_dots = iter(["ackd0", "ackd2"])
    rows = iter([0, 1, 0])                      # drawn row per pending entry (a hash has no order): A, B, B again

    def pop(t, acked):
        """DequeueWithAckId: LPOP head → pending + deadline + reply, one script (queue.go:481-501, 488, 498)."""
        k = lst.pop(0)
        row = next(rows)
        rows_free.remove(row)
        hx = SLOT[0]
        item[k] += [at(t, hx, HEAD[1], 1), at(t + .35, hx, ROW[row][1], 1), at(t + .7, *ROW[row], 1)]   # down, then left
        for i, other in enumerate(lst):          # the rest move up one slot once the head has left
            item[other] += [at(t + .35, SLOT[i + 1], HEAD[1], 1), at(t + .8, SLOT[i], HEAD[1], 1)]
        place(k, t, "pending")
        deadlines[k] = t + VT                  # Redis TIME + vt (queue.go:649-656)
        arrive = walk(next(dots), t, POP_EDGE)
        done = arrive + WORK
        busy.append((arrive, done))
        pops.append((t, k))
        if acked:                               # Acknowledge deletes entry + deadline (queue.go:353-373)
            a = walk(next(ack_dots), done, ACK_EDGE)
            item[k] += [at(a, *ROW[row], 1), at(a + .3, *ROW[row], 0)]
            place(k, a, "done")
            acks.append((a, k))
            bars[row].append((t, a, (a - t) / VT))   # deadline bar fills at the timeout's rate, stops at the ack
            assert a - t < VT, "acked before the deadline"
            rows_free.append(row)
            return done, a, row
        bars[row].append((t, t + VT))
        return done, None, row

    # A: popped at the first poll, acknowledged
    doneA, ackA, _ = pop(1.4, acked=True)
    # B: popped at the next poll, returned without Acknowledge
    doneB, _, rowB = pop(ackA + .3, acked=False)
    dlB = deadlines["B"]
    assert doneB < dlB, "B's run ends before its deadline: only the missing ack brings it back"
    flash("noack", [(doneB, doneB + 1.6)], 1)

    # ---- sweep: first tick at or after B's deadline moves B back to the list front (queue.go:827, 747) ----
    ticks = [k + SWEEP_PHASE for k in range(40)]
    req = next(s for s in ticks if s >= dlB)
    assert 0 <= req - dlB < 1, "requeue at the first sweep at/after the deadline"
    flash("late1", [(dlB, req)], 1)
    item["B"] += [at(req, *ROW[rowB], 1), at(req + .7, *HEAD, 1)]
    lst.insert(0, "B")
    place("B", req, "list")
    rows_free.append(rowB)
    flash("front", [(req, req + 1.6)], 1)

    # B again: next poll pops it under a new ack ID and deadline, then it is acknowledged
    doneB2, ackB2, rowB2 = pop(req + .9, acked=True)

    end = ackB2 + .3
    kfu.D = float(int(end + 1.5) + 1)           # whole seconds: the sweep's 1 s pulses repeat seamlessly
    assert end + 1 <= kfu.D

    # ---- invariants ----
    assert [k for _, k in pops] == ["A", "B", "B"], pops                     # B delivered twice, A once
    assert [k for _, k in acks] == ["A", "B"], acks
    for k, log in where.items():                                             # one place at a time, in order
        assert [p for _, p in log] == (["list", "pending", "done"] if k == "A" else
                                       ["list", "pending", "list", "pending", "done"]), (k, log)
        assert [t for t, _ in log] == sorted(t for t, _ in log), (k, log)
    for (s, e), (s2, _) in zip(busy, busy[1:]):                              # one job at a time
        assert e <= s2, busy
    assert not lst and sorted(rows_free) == [0, 1]                           # seam: list and pending empty
    assert all(e <= kfu.D for _, e in busy)

    # ---- render ----
    for k, pts in item.items():
        kf(f"item{k}", pts)
    assert rowB == 1 and rowB2 == 0, "the ack label sits on row 0: only A and B's second delivery use it"
    flash("ok0", [(a, a + 1.4) for a, _ in acks], 1)
    for row, spans in bars.items():
        bar(f"bar{row}", spans)
    flash("c-glow", busy, .14)
    bar("c-bar", busy)
    flash("enq-glow", [(t - .05, t + .3) for t in ENQ.values()], .25)
    ring("ring", [s for s in ticks if s < kfu.D])
    still = round(dlB - .8, 2)                                               # B's bar 80% full, A acked
    kfu.render("ack.tpl.svg", "../redisq-ack.svg", still=still)
    return pops, acks, dlB, req, busy


if __name__ == "__main__":
    pops, acks, dlB, req, busy = build()
    print("pops", [(round(t, 2), k) for t, k in pops], "acks", [(round(t, 2), k) for t, k in acks])
    print("B's first deadline", round(dlB, 2), "requeued at sweep", req, "busy", [(round(a, 2), round(b, 2)) for a, b in busy])
    print("D", kfu.D)
