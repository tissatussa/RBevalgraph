#!/usr/bin/env python3
"""
uci-eval-graph.py — run a UCI engine on one position with MultiPV and plot how
each candidate move's evaluation changed as the search got deeper.

Two modes:

  live    give --engine and --fen; the script drives the engine over UCI,
          stops it on your time/depth cap, and plots the result.
  replay  give --from-log (a file of raw engine stdout, e.g. one produced by a
          `tee` wrapper or by --save-log) and plot it without running anything.

Examples
--------
  ./uci-eval-graph.py --engine ./ember --multipv 6 --seconds 120 \
      --fen "6k1/5pp1/1p4rp/2p1n3/2q1P2P/2P2P2/1B1Q2P1/3R3K b - - 5 48"

  ./uci-eval-graph.py --engine /usr/games/stockfish --depth 24 --threads 6 \
      --hash 256 --fen "<fen>" --save-log sf.log --out sf.png

  ./uci-eval-graph.py --from-log uci-out.log --fen "<fen>" --out replay.png

Requires: matplotlib.  Optional: python-chess (for SAN move names; without it
the moves are labelled with their raw UCI strings).
"""

from __future__ import annotations

import argparse
import os
import queue
import subprocess
import sys
import threading
import time
from collections import defaultdict

try:
    import chess
except ImportError:                                   # SAN is a nicety, not a need
    chess = None

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D


# --------------------------------------------------------------------------- #
#  Talking to the engine
# --------------------------------------------------------------------------- #

class Engine:
    """Minimal UCI driver: line-based, with a reader thread so nothing deadlocks."""

    def __init__(self, path: str, echo: bool = False):
        self.proc = subprocess.Popen(
            [path],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
            cwd=os.path.dirname(os.path.abspath(path)) or None,
        )
        self.echo = echo
        self.lines: list[str] = []          # everything the engine said
        self.q: queue.Queue[str] = queue.Queue()
        self.options: dict[str, str] = {}   # option name -> type
        self.name = os.path.basename(path)
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()

    def _read(self):
        for line in self.proc.stdout:
            line = line.rstrip("\n")
            self.lines.append(line)
            self.q.put(line)
            if self.echo:
                print("<<", line, file=sys.stderr)

    def send(self, cmd: str):
        if self.echo:
            print(">>", cmd, file=sys.stderr)
        try:
            self.proc.stdin.write(cmd + "\n")
            self.proc.stdin.flush()
        except (BrokenPipeError, ValueError):
            raise SystemExit("error: the engine closed its input; it may have crashed")

    def wait_for(self, token: str, timeout: float) -> bool:
        """Wait for a line that starts with `token`. False on timeout."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                line = self.q.get(timeout=0.05)
            except queue.Empty:
                if self.proc.poll() is not None:
                    return False
                continue
            if line.split(" ")[0] == token:
                return True
        return False

    def handshake(self, timeout: float = 15.0):
        self.send("uci")
        if not self.wait_for("uciok", timeout):
            raise SystemExit("error: no 'uciok' from the engine — is it really UCI?")
        for line in self.lines:
            t = line.split()
            if t[:2] == ["option", "name"] and "type" in t:
                i = t.index("type")
                self.options[" ".join(t[2:i])] = t[i + 1]
        for line in self.lines:
            if line.startswith("id name "):
                self.name = line[len("id name "):].strip()

    def setoption(self, name: str, value: str, required: bool = False):
        """Set an option, warning instead of failing when the engine lacks it."""
        if name not in self.options:
            msg = f"warning: engine has no UCI option '{name}' — skipping"
            if required:
                msg += " (results will be single-PV)"
            print(msg, file=sys.stderr)
            return False
        self.send(f"setoption name {name} value {value}")
        return True

    def isready(self, timeout: float = 30.0):
        self.send("isready")
        if not self.wait_for("readyok", timeout):
            print("warning: no 'readyok' — continuing anyway", file=sys.stderr)

    def quit(self):
        try:
            self.send("quit")
            self.proc.wait(timeout=3)
        except Exception:
            pass
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def run_search(args) -> tuple[list[str], str, int]:
    """Drive the engine. Returns (output lines, engine name, index where stop was sent)."""
    eng = Engine(args.engine, echo=args.verbose)
    eng.handshake()

    eng.setoption("MultiPV", str(args.multipv), required=True)
    if args.hash is not None:
        eng.setoption("Hash", args.hash)
    if args.threads is not None:
        eng.setoption("Threads", str(args.threads))
    for item in args.setoption:
        if "=" not in item:
            raise SystemExit(f"error: --setoption wants NAME=VALUE, got {item!r}")
        name, value = item.split("=", 1)
        eng.setoption(name.strip(), value.strip())
    eng.isready()

    eng.send(f"position fen {args.fen}")
    eng.isready()

    # 'go depth N' is standard and almost universally supported, but it gives no
    # wall-clock guarantee: with MultiPV a single iteration can take minutes. So
    # the time cap is enforced here either way, and 'go infinite' is the default
    # because one stop path is easier to reason about than two.
    if args.go == "depth":
        if args.depth is None:
            raise SystemExit("error: --go depth needs --depth")
        eng.send(f"go depth {args.depth}")
    else:
        eng.send("go infinite")

    started = time.time()
    seen_full_set: set[int] = set()   # depths where all `multipv` slots arrived
    slots: dict[int, set[int]] = defaultdict(set)
    finished = False
    reason = "time limit"

    while True:
        elapsed = time.time() - started
        if elapsed >= args.seconds:
            reason = f"time limit ({args.seconds:g}s)"
            break
        try:
            line = eng.q.get(timeout=0.1)
        except queue.Empty:
            if eng.proc.poll() is not None:
                raise SystemExit("error: the engine exited during the search")
            continue

        if line.startswith("bestmove"):
            finished, reason = True, "engine returned bestmove"
            break
        if line.startswith("info ") and " pv " in line:
            rec = parse_info(line)
            if rec and rec.get("depth") is not None:
                slots[rec["depth"]].add(rec.get("multipv", 1))
                if len(slots[rec["depth"]]) >= args.multipv:
                    seen_full_set.add(rec["depth"])
                if args.depth is not None and max(seen_full_set, default=0) >= args.depth:
                    reason = f"depth limit ({args.depth})"
                    break

    stop_index = len(eng.lines)
    if not finished:
        eng.send("stop")
        if not eng.wait_for("bestmove", args.stop_grace):
            print(f"warning: no 'bestmove' within {args.stop_grace:g}s of 'stop' — "
                  "terminating the engine", file=sys.stderr)
    eng.quit()

    print(f"search ended: {reason}; {time.time() - started:.1f}s elapsed",
          file=sys.stderr)
    return eng.lines, eng.name, stop_index


# --------------------------------------------------------------------------- #
#  Parsing
# --------------------------------------------------------------------------- #

INT_FIELDS = {"depth", "seldepth", "multipv", "time", "nodes", "nps", "hashfull",
              "tbhits", "cpuload"}


def parse_info(line: str) -> dict | None:
    """Field-by-field parse of one `info ... pv ...` line. Order-independent."""
    t = line.split()
    if not t or t[0] != "info" or "pv" not in t:
        return None
    rec: dict = {}
    i = 1
    while i < len(t):
        tok = t[i]
        if tok == "pv":
            rec["pv"] = t[i + 1:]
            break
        if tok == "score":
            kind = t[i + 1]
            try:
                rec["score"] = (kind, int(t[i + 2]))
            except (IndexError, ValueError):
                return None
            i += 3
            continue
        if tok in ("lowerbound", "upperbound"):
            # Aspiration-window fail highs/lows are not evaluations. Drop the line.
            return None
        if tok in INT_FIELDS and i + 1 < len(t):
            try:
                rec[tok] = int(t[i + 1])
            except ValueError:
                pass
            i += 2
            continue
        i += 1
    if "pv" not in rec or not rec["pv"] or "score" not in rec:
        return None
    rec.setdefault("multipv", 1)
    rec.setdefault("time", 0)
    return rec


def collect(lines, mate_cap: float, drop_after: int | None):
    """Last reported value per (depth, multipv). Returns evals, ranks, times, mates."""
    last: dict[tuple[int, int], dict] = {}
    times: dict[int, int] = {}
    bestmove = None
    for n, line in enumerate(lines):
        if line.startswith("bestmove"):
            parts = line.split()
            if len(parts) > 1:
                bestmove = parts[1]
            continue
        if drop_after is not None and n >= drop_after:
            # Lines emitted after 'stop' are often a partial re-dump of the
            # interrupted iteration, with placeholder scores. Ignore them.
            continue
        if not line.startswith("info ") or " pv " not in line:
            continue
        rec = parse_info(line)
        if rec is None or rec.get("depth") is None:
            continue
        kind, raw = rec["score"]
        if kind == "mate" and raw == 0:
            continue                       # never a real evaluation
        last[(rec["depth"], rec["multipv"])] = rec
        times[rec["depth"]] = max(times.get(rec["depth"], 0), rec["time"])

    evals: dict[str, dict[int, float]] = defaultdict(dict)
    ranks: dict[str, dict[int, int]] = defaultdict(dict)
    mates: dict[str, dict[int, int]] = defaultdict(dict)
    for (depth, mpv), rec in last.items():
        mv = rec["pv"][0]
        kind, raw = rec["score"]
        if kind == "mate":
            evals[mv][depth] = mate_cap if raw > 0 else -mate_cap
            mates[mv][depth] = raw
        else:
            evals[mv][depth] = raw / 100.0
        ranks[mv][depth] = mpv
    return evals, ranks, times, mates, bestmove


def san_names(moves, fen):
    """UCI -> SAN where python-chess is available and the FEN is known."""
    names = {mv: mv for mv in moves}
    if chess is None or not fen:
        return names, False
    try:
        board = chess.Board(fen)
    except ValueError:
        print("warning: could not parse the FEN — labelling with UCI moves",
              file=sys.stderr)
        return names, False
    for mv in moves:
        try:
            names[mv] = board.san(chess.Move.from_uci(mv))
        except Exception:
            names[mv] = mv
    return names, True


# --------------------------------------------------------------------------- #
#  Plotting
# --------------------------------------------------------------------------- #

PALETTE = ["#d62728", "#1f77b4", "#2ca02c", "#ff7f0e", "#9467bd", "#8c564b",
           "#17becf", "#bcbd22", "#e377c2", "#7f7f7f", "#393b79", "#b5651d",
           "#5254a3", "#8ca252", "#ad494a", "#c49c94", "#6b6ecf", "#e7969c"]


def plot(evals, ranks, times, mates, bestmove, names, args, engine_name, meta_line):
    depths = sorted(times)
    if not depths:
        raise SystemExit("error: no usable 'info ... pv ...' lines found")
    # A search stopped mid-iteration leaves the deepest depth with only some of
    # its PV slots filled. Rank the candidates by the last *complete* iteration,
    # while still plotting whatever the partial one produced.
    per_depth = {d: sum(1 for mv in evals if d in evals[mv]) for d in depths}
    width = min(args.multipv, max(per_depth.values()))
    complete = [d for d in depths if per_depth[d] >= width]
    final_depth = max(complete) if complete else depths[-1]
    stop_s = times[depths[-1]] / 1000.0

    final_set = sorted((mv for mv in evals if final_depth in evals[mv]),
                       key=lambda mv: ranks[mv][final_depth])
    others = sorted((mv for mv in evals if mv not in final_set),
                    key=lambda mv: (-len(evals[mv]), -max(evals[mv].values())))
    order = final_set + others
    colour = {mv: PALETTE[i % len(PALETTE)] for i, mv in enumerate(order)}

    sign = -1.0 if args.white_pov else 1.0

    # Candidates that share an eval would otherwise be drawn on top of each
    # other. Fan them a few pixels apart, keeping a stable order per group.
    shift = defaultdict(dict)
    for d in depths:
        tied = defaultdict(list)
        for mv in order:
            if d in evals[mv]:
                tied[round(evals[mv][d], 4)].append(mv)
        for _v, group in tied.items():
            n = len(group)
            for i, mv in enumerate(group):
                shift[mv][d] = (i - (n - 1) / 2.0) * args.nudge

    def y_of(mv, d):
        return sign * evals[mv][d] + shift[mv][d]

    fig = plt.figure(figsize=(args.width, args.height))
    fig.patch.set_facecolor("white")
    ax = fig.add_axes([0.055, 0.085, 0.80, 0.705])

    for mv in order:
        top = mv in final_set
        col = colour[mv]
        d = sorted(evals[mv])
        y = [y_of(mv, x) for x in d]
        lw = 2.4 if top else 1.3

        chunk_x, chunk_y = [d[0]], [y[0]]
        for xa, ya in zip(d[1:], y[1:]):
            if xa == chunk_x[-1] + 1:
                chunk_x.append(xa)
                chunk_y.append(ya)
            else:
                ax.plot(chunk_x, chunk_y, color=col, lw=lw,
                        alpha=1.0 if top else 0.8, zorder=4 if top else 3)
                chunk_x, chunk_y = [xa], [ya]
        ax.plot(chunk_x, chunk_y, color=col, lw=lw, alpha=1.0 if top else 0.8,
                zorder=4 if top else 3)
        ax.plot(d, y, "o", color=col, ms=4.2 if top else 2.8, mec="white",
                mew=0.7, zorder=5)

        if not top:
            ax.annotate(names[mv], xy=(d[-1], y[-1]), xytext=(5, 0),
                        textcoords="offset points", va="center", fontsize=8.5,
                        color=col)

    if bestmove in evals:
        bd = max(evals[bestmove])
        ax.plot([bd], [y_of(bestmove, bd)], marker="*", ms=20,
                color=colour[bestmove], mec="black", mew=0.9, zorder=6)
        ax.annotate(f"bestmove {names[bestmove]}",
                    xy=(bd, y_of(bestmove, bd)),
                    xytext=(-26, -30), textcoords="offset points", ha="right",
                    fontsize=11, fontweight="bold", color=colour[bestmove],
                    arrowprops=dict(arrowstyle="-", color=colour[bestmove], lw=1.0))

    lo = min(y_of(m, d) for m in evals for d in evals[m])
    hi = max(y_of(m, d) for m in evals for d in evals[m])
    pad = max(0.06, (hi - lo) * 0.06)
    ax.set_ylim(lo - pad, hi + pad * 1.6)
    ax.axvline(final_depth, color="black", lw=0.9, ls="--", alpha=0.45)
    ax.axhline(0, color="black", lw=0.8, alpha=0.5)
    ax.set_xlabel("Search depth (ply)", fontsize=12)
    side = "White" if args.white_pov else "the side to move"
    ax.set_ylabel(f"Evaluation in pawns  (+ = good for {side})", fontsize=12)
    step = 1 if len(depths) <= 26 else 2
    ticks = [d for d in depths if d % step == 0] or depths
    ax.set_xticks(ticks)
    ax.set_xlim(depths[0] - 0.5, depths[-1] + 0.8)
    ax.grid(True, alpha=0.25, lw=0.7)
    ax.set_axisbelow(True)

    secax = ax.twiny()
    secax.set_xlim(ax.get_xlim())
    secax.set_xticks(ticks)
    secax.set_xticklabels(
        [f"{times[d]/1000:.0f}" if times.get(d, 0) >= 1000 else "" for d in ticks],
        fontsize=8, color="#555555")
    secax.set_xlabel("elapsed search time at that depth (seconds)",
                     fontsize=10, color="#555555", labelpad=8)

    fig.text(0.465, 0.962,
             f"{engine_name} — MultiPV {args.multipv}: how each candidate move's "
             r"$\bf{eval\ changed}$" " with rising depth in time",
             ha="center", va="top", fontsize=14)
    pov = ("Values are shown from White's side, as SCID displays them."
           if args.white_pov else
           "Values are as the engine reports them, from the side to move; "
           "SCID displays these negated.")
    fig.text(0.465, 0.925, pov, ha="center", va="top", fontsize=12)
    if args.fen:
        fig.text(0.055, 0.893, f"FEN   {args.fen}", fontsize=9,
                 family="monospace", color="#333333", va="top")
    fig.text(0.055, 0.871, meta_line, fontsize=9, family="monospace",
             color="#333333", va="top")
    if args.nudge:
        fig.text(0.055, 0.849,
                 "candidates on the same eval are fanned a few pixels apart to "
                 "stay readable — the column on the right holds the true values",
                 fontsize=8.5, style="italic", color="#666666", va="top")

    key = fig.add_axes([0.875, 0.085, 0.115, 0.705])
    key.set_axis_off()
    key.set_xlim(0, 1)
    key.set_ylim(len(order) + 0.5, -0.5)
    for i, mv in enumerate(order):
        top = mv in final_set
        key.add_line(Line2D([0.02, 0.20], [i, i], color=colour[mv],
                            lw=2.4 if top else 1.3, alpha=1.0 if top else 0.8,
                            transform=key.transData, clip_on=False))
        key.text(0.27, i, names[mv], va="center", fontsize=11.5 if top else 9.5,
                 fontweight="bold" if top else "normal", color=colour[mv])
        d_last = max(evals[mv])
        if d_last in mates[mv]:
            label = f"#{mates[mv][d_last]:+d}"
        else:
            label = f"{sign * evals[mv][d_last]:+.2f}"
        key.text(1.0, i, label, va="center", ha="right",
                 fontsize=11.5 if top else 9.5,
                 fontweight="bold" if top else "normal", color=colour[mv])
        if i == len(final_set) - 1 and others:
            key.add_line(Line2D([0.0, 1.0], [i + 0.5, i + 0.5], color="#333333",
                                lw=1.0, transform=key.transData, clip_on=False))

    fig.savefig(args.out, dpi=args.dpi, facecolor="white")
    plt.close(fig)
    print(f"wrote {args.out}  ({len(order)} candidate moves, "
          f"depth {depths[0]}–{depths[-1]}, {stop_s:.1f}s)")
    return order, depths


def print_table(order, depths, evals, times, names, sign):
    head = "depth " + " ".join(f"{names[m]:>6}" for m in order)
    print(head)
    for d in depths:
        row = " ".join(f"{sign * evals[m][d]:>6.2f}" if d in evals[m] else "     ·"
                       for m in order)
        print(f"{d:>5} {row}   t={times[d]/1000:.1f}s")


# --------------------------------------------------------------------------- #
#  Command line
# --------------------------------------------------------------------------- #

def main():
    p = argparse.ArgumentParser(
        description="Run a UCI engine on one FEN with MultiPV and graph how each "
                    "candidate move's eval changed with depth.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter)
    p.add_argument("--engine", help="path to the UCI engine binary")
    p.add_argument("--fen", help="position to analyse (needed for SAN names)")
    p.add_argument("--multipv", type=int, default=6, help="number of PV lines")
    p.add_argument("--seconds", type=float, default=60.0,
                   help="wall-clock cap; the search is stopped after this")
    p.add_argument("--depth", type=int, default=None,
                   help="stop once this depth has been completed for every PV")
    p.add_argument("--go", choices=("infinite", "depth"), default="infinite",
                   help="which go command to send")
    p.add_argument("--hash", default=None,
                   help="value for the Hash option, verbatim (e.g. 256 or 256Mb)")
    p.add_argument("--threads", type=int, default=None, help="value for Threads")
    p.add_argument("--setoption", action="append", default=[], metavar="NAME=VALUE",
                   help="any further UCI option; repeatable")
    p.add_argument("--stop-grace", type=float, default=10.0,
                   help="seconds to wait for bestmove after sending stop")
    p.add_argument("--from-log", help="replay a saved engine-output log instead")
    p.add_argument("--keep-post-stop", action="store_true",
                   help="keep info lines emitted after stop (usually placeholders)")
    p.add_argument("--save-log", help="write the raw engine output here")
    p.add_argument("--out", default="eval-by-depth.png", help="PNG to write")
    p.add_argument("--white-pov", action="store_true",
                   help="negate scores so + means good for White, like SCID")
    p.add_argument("--mate-cap", type=float, default=10.0,
                   help="pawn value used to plot a mate score")
    p.add_argument("--nudge", type=float, default=0.011,
                   help="vertical fan applied to equal evals; 0 disables it")
    p.add_argument("--width", type=float, default=15.5)
    p.add_argument("--height", type=float, default=8.8)
    p.add_argument("--dpi", type=int, default=170)
    p.add_argument("--table", action="store_true", help="also print the values")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="echo the UCI conversation to stderr")
    args = p.parse_args()

    if not args.engine and not args.from_log:
        p.error("give either --engine (live) or --from-log (replay)")
    if args.engine and not args.from_log and not args.fen:
        p.error("--fen is required for a live run")

    if args.from_log:
        with open(args.from_log, encoding="utf-8", errors="replace") as fh:
            lines = [ln.rstrip("\n") for ln in fh]
        engine_name = next((ln[len("id name "):].strip() for ln in lines
                            if ln.startswith("id name ")), os.path.basename(args.from_log))
        stop_index = None
        meta = f"replayed from {os.path.basename(args.from_log)}"
    else:
        if not os.path.isfile(args.engine) or not os.access(args.engine, os.X_OK):
            raise SystemExit(f"error: {args.engine} is not an executable file")
        lines, engine_name, stop_index = run_search(args)
        bits = [f"go {args.go}", f"MultiPV {args.multipv}"]
        if args.hash is not None:
            bits.append(f"Hash {args.hash}")
        if args.threads is not None:
            bits.append(f"Threads {args.threads}")
        bits.append(f"cap {args.seconds:g}s"
                    + (f" / depth {args.depth}" if args.depth else ""))
        meta = "  ·  ".join(bits)
        if args.save_log:
            with open(args.save_log, "w", encoding="utf-8") as fh:
                fh.write("\n".join(lines) + "\n")
            print(f"wrote {args.save_log}")

    evals, ranks, times, mates, bestmove = collect(
        lines, args.mate_cap, None if args.keep_post_stop else stop_index)
    if not evals:
        raise SystemExit("error: no usable 'info ... pv ...' lines in the output")

    names, have_san = san_names(list(evals), args.fen)
    if not have_san:
        print("note: labelling moves with UCI strings "
              "(install python-chess and pass --fen for SAN)", file=sys.stderr)

    order, depths = plot(evals, ranks, times, mates, bestmove, names, args,
                         engine_name, meta)
    if args.table:
        print_table(order, depths, evals, times, names,
                    -1.0 if args.white_pov else 1.0)


if __name__ == "__main__":
    main()
