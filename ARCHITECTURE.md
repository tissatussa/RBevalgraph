# RBevalgraph — architecture notes

For coders who want to read or change `main.go`, and for future reference. The comments in `main.go` carry the details; this is the map.

## One file, on purpose

About 3300 lines in a single `main.go`, so the program builds with `go build` and nothing else. The sections appear in this order:

| Section | Main names | What it does |
|---|---|---|
| search state | `search`, `note`, `depths`, `lastComplete`, `ordered`, `reset` | Everything the engine reported. Shared between the engine goroutine and the drawing code, guarded by one mutex. |
| info parsing | `infoLine`, `parseInfo`, `pickElapsed` | Reads UCI `info` lines. |
| engine probing | `uciOption`, `parseOptionLine`, `probeEngine`, `probeXBoard`, `probeIdle`, `probeMax` | Identifies the engine and its options. |
| position | `parseFENBoard`, `checkFEN`, `sanFor` | FEN handling, SAN names (via notnil/chess). |
| texts and icons | `aboutText`, `infoText`, `infoSVG`, `toggleSVG`, `aboutSVG`, `iconButton` | Pane texts (Pango markup) and inlined button icons. |
| pieces | `pieceSet`, `alphaSet`, `unicodeSet`, `pieces` | Font choice for the diagram. |
| engine driver | `runConfig`, `runner`, `start` | One goroutine per search, talking over pipes, posting `glib.IdleAdd(redraw)`. |
| look | `palette`, `nudgePx`, `pngScale`, `cornerRadius`, `quantile` | Constants of the drawing. |
| config | `config`, `configPath`, `defaultConfig`, `loadConfig`, `save` | `config.yaml` beside the binary. |
| drawing | `draw`, `drawBoard`, `drawMoveArrow`, `strokePolyline` | The whole canvas. |
| output | `sanitize`, `writeSearchLog`, `suggestedPNGName` | File names, the search log. |
| UI | `main`, `buildUI` | Every widget, the 500 ms timer (validation, probing, config saving) and the overlay pane. |

`draw()` is a pure function of the `search` state plus a width and height; it touches no widgets. That is why the PNG export is trivial: the same function, drawn on a bigger surface.

## Decisions that look odd but are deliberate

Read the reason before "fixing" one of these.

- **gotk4 is pinned to v0.3.1.** v0.4.x needs GLib 2.82+; Ubuntu 24.04 has 2.80.
- **The options/INFO/About pane is an overlay, not a window.** GTK4 removed window positioning (Wayland forbids it), so a separate window cannot be centred on the app. As an overlay child it is centred by construction. For the same reason the main window is not centred on screen — that is a window-manager setting.
- **The engine field is masked, not overwritten, during detection.** The real path lives in `maskedPath`; every reader goes through `enginePath()`. Writing the status message into the entry would save it to `config.yaml` as the engine path.
- **Digit-only fields use an allow-list by keyval range.** Everything ≥ `0xff00` passes (editing and navigation keys), plus `ISO_Left_Tab` (Shift+Tab), which sits just below that range.
- **Probe timeouts are idle timeouts, not totals.** Any output resets the clock; `feature done=0` from an XBoard engine suspends it. Neither UCI nor CECP defines a number.
- **Info parsing:** the last line per (depth, multipv) wins; `lowerbound`/`upperbound` lines are dropped (aspiration-window fails, not evaluations); `mate 0` is dropped (a placeholder some engines send at stop); lines after `stop` are ignored.
- **`pickElapsed`** prefers the engine's `time` field but falls back to our own clock when it is missing or off by more than 4×, because some old engines report centiseconds.
- **The vertical scale is symlog, centred on the data:** median as centre, 75th percentile of absolute deviations as the spread, linear within one spread, logarithmic beyond, the cluster in the middle 62% of the height.
- **Ties are fanned apart in pixels,** not in evaluation units, because the axis is non-linear.
- **Lines are corner-rounded, not spline-smoothed** — a spline overshoots and shows values the engine never reported.
- **Colour follows rank.** A move takes the colour of the place it holds, so it changes colour when it overtakes another.
- **Chess font detection is by metrics** (advance = em, no descender), because Chess Alpha maps pieces onto ASCII letters and a silent font substitution would print letters on the board. Alpha's mapping, read off the font's own glyphs: white `K=k Q=q R=r B=b N=h P=p`, black `k=l q=w r=t b=n n=j p=o`. A white piece is its solid glyph in white with the outline glyph on top.
- **Board geometry:** board width = key column width − 24, so its right edge lines up with the evaluation figures; the rank numbers sit in a 14 px gutter outside it. Pieces are drawn at 0.86 of the square.

## Known rough edges

- A `GtkText - did not receive a focus-out event` warning may appear once at startup. Harmless.
- `Theme parser error: gtk.css:…` lines in the terminal come from a GTK3 theme being read by GTK4, not from RBevalgraph.
- The CSS uses `@theme_base_color`, a GTK3-era named colour. A GTK4-native theme may not define it, and the pane would lose its background.
- With an empty config, the first run shows only `[ no UCI engine selected ]` until an engine is loaded.
- Windows and macOS builds are untested. The code has no Linux-only parts (the one fontconfig call, `fc-list`, falls back when it is missing), but a Windows build is an exe that needs the MSYS2 GTK4 DLLs, loaders and an icon theme beside it or on the `PATH`, rather than one self-contained file.

## Testing without a GUI

The pure-Go parts (UCI parsing, config, FEN, file names, timings) can be tested outside GTK: copy the block into a scratch module and run it against saved engine logs. Fake engines as small shell scripts — a slow UCI engine, an XBoard engine, a binary that never answers — exercise the probing code. `tools/uci-eval-graph.py --from-log` re-plots a saved log for comparison.
