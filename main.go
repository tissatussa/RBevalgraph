// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Roelof Berkepeis
//
// rbevalgraph — a small GTK4 front end that runs a UCI chess engine on one
// position with MultiPV and plots, live, how every candidate move's evaluation
// changes as the search deepens.
//
// Build:
//
//	go mod tidy
//	go build -o rbevalgraph .
//
// Needs GTK4 development files (libgtk-4-dev) and cgo.
package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/notnil/chess"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// shared state between the engine goroutine and the drawing code
// ---------------------------------------------------------------------------

type search struct {
	mu sync.Mutex

	evals map[string]map[int]float64 // uci move -> depth -> pawns, side to move
	ranks map[string]map[int]int     // uci move -> depth -> multipv slot
	mates map[string]map[int]int     // uci move -> depth -> mate distance
	times map[int]int                // depth -> ms elapsed at that depth
	seen  []string                   // first-seen order, keeps colours stable
	san   map[string]string          // uci move -> SAN (or uci if unavailable)

	board      [8][8]rune // rank 8 first, file a first; 0 is an empty square
	sideToMove byte       // 'w' or 'b'
	flipped    bool       // true when Black is at the bottom

	// True while an engine is being asked what protocol it speaks.
	probing bool

	// Where the pointer is over the canvas, and whether it is there at all.
	hoverX, hoverY float64
	hovering       bool

	best      string
	engineID  string
	meta      string
	fen       string
	multipv   int
	maxDepth  int
	maxSecs   float64
	status    string
	running   bool
	started   time.Time
	stoppedAt time.Duration
}

func newSearch() *search {
	s := &search{}
	s.reset()
	s.status = "idle — pick an engine and press Generate eval graph"
	return s
}

func (s *search) reset() {
	s.evals = map[string]map[int]float64{}
	s.ranks = map[string]map[int]int{}
	s.mates = map[string]map[int]int{}
	s.times = map[int]int{}
	s.seen = nil
	s.san = map[string]string{}
	s.best = ""
	s.stoppedAt = 0
	s.started = time.Time{}
}

// note records one reported line. engineMS is the engine's own "time" field
// and wallMS is our own measurement since the go command; see pickElapsed.
func (s *search) note(mv string, depth, mpv int, cp float64, mate int, isMate bool, engineMS, wallMS int) {
	if _, ok := s.evals[mv]; !ok {
		s.evals[mv] = map[int]float64{}
		s.ranks[mv] = map[int]int{}
		s.mates[mv] = map[int]int{}
		s.seen = append(s.seen, mv)
	}
	s.evals[mv][depth] = cp
	s.ranks[mv][depth] = mpv
	if isMate {
		s.mates[mv][depth] = mate
	} else {
		delete(s.mates[mv], depth)
	}
	if ms := pickElapsed(engineMS, wallMS); ms > s.times[depth] {
		s.times[depth] = ms
	}
}

// pickElapsed decides which clock to believe. UCI says "time" is the total
// milliseconds spent on the current search, so it is cumulative already and
// the value at a depth's last line is when that iteration finished. Not every
// engine fills it in, though, and some old ones report centiseconds or
// seconds. So the engine's figure is used only when it is present and within a
// factor of four of our own measurement; otherwise we fall back to our clock,
// which starts when "go" was sent and is never absent.
func pickElapsed(engineMS, wallMS int) int {
	if engineMS <= 0 {
		return wallMS
	}
	if wallMS <= 0 {
		return engineMS
	}
	ratio := float64(engineMS) / float64(wallMS)
	if ratio < 0.25 || ratio > 4 {
		return wallMS
	}
	return engineMS
}

// depths returns every depth that has data, ascending.
func (s *search) depths() []int {
	out := make([]int, 0, len(s.times))
	for d := range s.times {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// lastComplete is the deepest iteration where every PV slot reported. A search
// stopped mid-iteration leaves the final depth half-filled, and ranking the
// candidates on that would be misleading.
func (s *search) lastComplete() int {
	ds := s.depths()
	if len(ds) == 0 {
		return 0
	}
	width := 0
	for _, d := range ds {
		n := 0
		for _, m := range s.seen {
			if _, ok := s.evals[m][d]; ok {
				n++
			}
		}
		if n > width {
			width = n
		}
	}
	if s.multipv > 0 && width > s.multipv {
		width = s.multipv
	}
	best := ds[0]
	for _, d := range ds {
		n := 0
		for _, m := range s.seen {
			if _, ok := s.evals[m][d]; ok {
				n++
			}
		}
		if n >= width {
			best = d
		}
	}
	return best
}

// ordered returns the candidates: the ones present at the last complete
// iteration first, in engine order, then everything else by how long it
// survived. The second value is how many belong to the top group.
func (s *search) ordered() ([]string, int) {
	fd := s.lastComplete()
	var top, rest []string
	for _, m := range s.seen {
		if _, ok := s.evals[m][fd]; ok && fd != 0 {
			top = append(top, m)
		} else {
			rest = append(rest, m)
		}
	}
	// Engines do not always emit their PV lines in order, so sort by the
	// evaluation itself and fall back to the slot number for exact ties.
	sort.SliceStable(top, func(i, j int) bool {
		a, b := s.evals[top[i]][fd], s.evals[top[j]][fd]
		if a != b {
			return a > b
		}
		return s.ranks[top[i]][fd] < s.ranks[top[j]][fd]
	})
	// The rest are ranked by the last evaluation each of them was given,
	// deepest first when those are equal.
	sort.SliceStable(rest, func(i, j int) bool {
		di, dj := lastDepth(s.evals[rest[i]]), lastDepth(s.evals[rest[j]])
		vi, vj := s.evals[rest[i]][di], s.evals[rest[j]][dj]
		if vi != vj {
			return vi > vj
		}
		return di > dj
	})
	return append(top, rest...), len(top)
}

func lastDepth(m map[int]float64) int {
	out := 0
	for d := range m {
		if d > out {
			out = d
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// UCI parsing
// ---------------------------------------------------------------------------

type infoLine struct {
	depth   int
	multipv int
	cp      float64
	mate    int
	isMate  bool
	timeMS  int
	move    string
	ok      bool
}

func parseInfo(line string) infoLine {
	var out infoLine
	t := strings.Fields(line)
	if len(t) == 0 || t[0] != "info" {
		return out
	}
	out.multipv = 1
	for i := 1; i < len(t); i++ {
		switch t[i] {
		case "lowerbound", "upperbound":
			// Aspiration fail high/low: not an evaluation, drop the whole line.
			return infoLine{}
		case "pv":
			if i+1 < len(t) {
				out.move = t[i+1]
			}
			i = len(t)
		case "score":
			if i+2 < len(t) {
				v, err := strconv.Atoi(t[i+2])
				if err == nil {
					switch t[i+1] {
					case "cp":
						out.cp = float64(v) / 100
					case "mate":
						out.isMate, out.mate = true, v
					}
				}
				i += 2
			}
		case "depth", "multipv", "time":
			if i+1 < len(t) {
				v, err := strconv.Atoi(t[i+1])
				if err == nil {
					switch t[i] {
					case "depth":
						out.depth = v
					case "multipv":
						out.multipv = v
					case "time":
						out.timeMS = v
					}
				}
				i++
			}
		}
	}
	out.ok = out.depth > 0 && out.move != ""
	return out
}

// hashCeiling caps the Hash spin button: large enough for any real engine,
// small enough that the value always fits the little box.
const hashCeiling float64 = 640000

// How long an engine is given to identify itself. Neither protocol defines a
// number: UCI only says the GUI kills the task "if no uciok is sent within a
// certain time period", and CECP's timeout exists solely to spot a protocol-v1
// engine that will never answer "protover". So rather than a fixed total, the
// clock here is an idle one — any line at all proves the engine is alive and
// resets it — under an absolute ceiling. That lets a slow starter load its
// books or nets while a dead binary still fails quickly.
const (
	probeIdle = 10 * time.Second // silence that means "it is not answering"
	probeMax  = 60 * time.Second // ceiling, however talkative it is
)

// uciOption is one line of an engine's advertised option list.
type uciOption struct {
	typ      string
	def      string
	min, max float64 // spin bounds; both zero when the engine gave none
	hasRange bool
	vars     []string // combo choices, in the order the engine gave them
}

// parseOptionLine reads "option name X type spin default 1 min 1 max 64" and
// "option name Hash type combo default auto-size var auto-size var 4Mb ...".
func parseOptionLine(line string) (string, uciOption, bool) {
	if !strings.HasPrefix(line, "option name ") {
		return "", uciOption{}, false
	}
	rest := line[len("option name "):]
	i := strings.Index(rest, " type ")
	if i < 0 {
		return "", uciOption{}, false
	}
	name := rest[:i]
	t := strings.Fields(rest[i+len(" type "):])
	if len(t) == 0 {
		return "", uciOption{}, false
	}
	opt := uciOption{typ: t[0]}
	for j := 1; j < len(t); j++ {
		switch t[j] {
		case "default":
			var parts []string
			for j+1 < len(t) && t[j+1] != "var" && t[j+1] != "min" && t[j+1] != "max" {
				j++
				parts = append(parts, t[j])
			}
			opt.def = strings.Join(parts, " ")
		case "min", "max":
			if j+1 < len(t) {
				which := t[j]
				j++
				if v, err := strconv.ParseFloat(t[j], 64); err == nil {
					if which == "min" {
						opt.min = v
					} else {
						opt.max = v
					}
					opt.hasRange = true
				}
			}
		case "var":
			if j+1 < len(t) {
				j++
				opt.vars = append(opt.vars, t[j])
			}
		}
	}
	return name, opt, true
}

// probeEngine starts the binary, asks it for its option list and quits again.
// Any failure just yields no options, and the caller greys the controls out
// rather than pretending they will work.
// probeResult is what one "uci" handshake told us.
type probeResult struct {
	options map[string]uciOption
	name    string        // from "id name"
	raw     []string      // everything the engine said, verbatim and in order
	took    time.Duration // how long it needed to answer
}

func probeEngine(path string) (probeResult, error) {
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	start := time.Now()
	res := probeResult{options: map[string]uciOption{}}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return res, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, err
	}
	defer func() {
		io.WriteString(stdin, "quit\n")
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
		}
	}()

	if _, err := io.WriteString(stdin, "uci\n"); err != nil {
		return res, err
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<16)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	idle := time.NewTimer(probeIdle)
	defer idle.Stop()
	overall := time.After(probeMax)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return res, fmt.Errorf("it stopped without answering 'uci'")
			}
			resetTimer(idle, probeIdle) // it is alive; start the clock again
			if n, opt, ok := parseOptionLine(line); ok {
				res.options[n] = opt
			}
			// The pane shows the whole reply — the banner, id name, id author
			// and the options — not just the lines we happen to parse.
			res.raw = append(res.raw, strings.TrimRight(line, " \t\r"))
			if strings.HasPrefix(line, "id name ") {
				res.name = strings.TrimSpace(line[len("id name "):])
			}
			if strings.TrimSpace(line) == "uciok" {
				res.took = time.Since(start)
				return res, nil
			}
		case <-idle.C:
			return res, fmt.Errorf("it said nothing for %v", probeIdle)
		case <-overall:
			return res, fmt.Errorf("it never sent 'uciok' (gave it %v)", probeMax)
		}
	}
}

// parseFENBoard turns the placement and side-to-move fields of a FEN into a
// grid for the diagram. The FEN is validated elsewhere; anything unexpected
// here simply yields an empty board and no diagram is drawn.
func parseFENBoard(fen string) ([8][8]rune, byte) {
	var board [8][8]rune
	fields := strings.Fields(fen)
	if len(fields) < 1 {
		return board, 'w'
	}
	rank := 0
	file := 0
	for _, c := range fields[0] {
		switch {
		case c == '/':
			rank++
			file = 0
			if rank > 7 {
				return board, 'w'
			}
		case c >= '1' && c <= '8':
			file += int(c - '0')
		default:
			if file > 7 || rank > 7 {
				return board, 'w'
			}
			board[rank][file] = c
			file++
		}
	}
	side := byte('w')
	if len(fields) > 1 && strings.HasPrefix(fields[1], "b") {
		side = 'b'
	}
	return board, side
}

// The two button icons, inlined so the program stays one file. They are the
// SVGs as supplied; the fill colours are their own.
const infoSVG = `<?xml version="1.0" encoding="utf-8"?>
<svg version="1.1" xmlns="http://www.w3.org/2000/svg" width="64px" height="64px" viewBox="0 0 100 100">
<g><path fill="#231F20" d="M50,12.5c-20.712,0-37.5,16.793-37.5,37.502C12.5,70.712,29.288,87.5,50,87.5
c20.712,0,37.5-16.788,37.5-37.498C87.5,29.293,70.712,12.5,50,12.5z M53.826,70.86c0,0.72-0.584,1.304-1.304,1.304h-5.044
c-0.72,0-1.304-0.583-1.304-1.304V46.642c0-0.72,0.584-1.304,1.304-1.304h5.044c0.72,0,1.304,0.583,1.304,1.304V70.86z
M49.969,39.933c-2.47,0-4.518-2.048-4.518-4.579c0-2.53,2.048-4.518,4.518-4.518c2.531,0,4.579,1.987,4.579,4.518
C54.549,37.885,52.5,39.933,49.969,39.933z"/></g></svg>`

const toggleSVG = `<?xml version="1.0" encoding="utf-8"?>
<svg fill="#000000" version="1.1" xmlns="http://www.w3.org/2000/svg" width="64px" height="64px" viewBox="0 0 100 100">
<g><path d="M50,12.5c-20.712,0-37.5,16.793-37.5,37.502C12.5,70.712,29.288,87.5,50,87.5c20.712,0,37.5-16.788,37.5-37.498
C87.5,29.293,70.712,12.5,50,12.5z M50.124,22.443C65.265,22.51,77.56,34.848,77.56,50.002c0,15.155-12.295,27.488-27.436,27.555
V22.443z"/></g></svg>`

// aboutText is Pango markup: the first line bold and a size up, then the
// credit line. The pane centres both.
const aboutText = `<span size="x-large" weight="bold">RBevalgraph v1.0</span>

Enter any chess position - this program shows how each candidate move's ` +
	`evaluation changed with rising depth, using MultiPV.

Roelof Berkepeis - Holland - 2026@ArtFlow`

// infoText is the general explanation behind the INFO button. Pango markup:
// the title is bold and a size up, the section headings plain bold.
const infoText = `<span size="x-large" weight="bold">RBevalgraph</span>

<span weight="bold">Function</span>
RBevalgraph runs a UCI chess engine on any position and shows data of the ` +
	`changing candidate moves. While the MultiPV search runs, colored lines ` +
	`are plotted for all moves (ever considered by the engine), reflecting ` +
	`the evaluation fluctuations as depth rises.

<span weight="bold">Usage</span>
Browse to a UCI engine, paste a FEN, set your options and press 'Generate ` +
	`eval graph'. After the process ends, or is stopped, you can save the ` +
	`result as a PNG image. Many elements have a tooltip, hover for info. ` +
	`You can also manually enter values.

<span weight="bold">Saving the graph</span>
What you see is what you save : resizing the window results in resizing the ` +
	`graph and the saved PNG has same aspect ratio. The right info column ` +
	`keeps its width. The file name is proposed for you, mentioning the ` +
	`engine name and your main settings.

<span weight="bold">Configuration</span>
Options are set automatically according to engine features, or grayed-out ` +
	`when not existing. Everything you set is instantly written to a file ` +
	`called 'config.yaml', created in same folder as your RBevalgraph ` +
	`binary. This configuration file is optional, it's read when the program ` +
	`starts : your last settings are kept.

<span weight="bold">More info</span>
This project is maintained at ` +
	`<a href="https://github.com/tissatussa/RBevalgraph">github.com/tissatussa/RBevalgraph</a>`

// How the pane lays its body out.
const (
	paneMono    = iota // the engine's option list: monospace, top-left, no wrap
	paneCentred        // About: proportional, centred both ways
	paneProse          // INFO: wrapped paragraphs from the top left
)

const aboutSVG = `<?xml version="1.0" encoding="utf-8" ?><!-- created by svgstack.com | Attribution is required. --><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="1024" height="1024"><defs><linearGradient id="gradient_0" gradientUnits="userSpaceOnUse" x1="386.78171" y1="124.20273" x2="118.11049" y2="391.87399"><stop offset="0" stop-color="#B3526D"/><stop offset="1" stop-color="#002992"/></linearGradient></defs><path fill="url(#gradient_0)" transform="scale(2 2)" d="M209.064 64.4644C247.102 51.7083 304.438 60.1011 339.305 77.441C339.904 77.7389 340.533 78.0144 341.095 78.3767C370.306 91.6591 398.71 117.535 416.519 143.598C423.932 154.446 434.017 171.171 437.658 183.731C452.243 219.46 454.522 260.202 446.447 297.763C444.782 305.509 443.129 314.206 439.681 321.375C438.694 327.591 427.166 349.078 423.913 354.587C370.4 445.207 249.078 476.744 158.469 422.849C72.9811 371.999 36.3656 268.503 77.5693 175.461C101.806 120.734 151.169 79.166 209.064 64.4644Z"/><path fill="#FEFEFE" transform="scale(2 2)" d="M253.132 127.827C288.233 125.092 292.197 175.602 259.736 179.756C226.326 184.425 217.48 133.231 253.132 127.827Z"/><path fill="#B3526D" transform="scale(2 2)" d="M437.658 183.731C437.093 186.255 441.045 192.67 441.241 196.01L440.973 196.255C438.099 189.881 424.647 175.994 418.635 171.612C404.852 161.566 392.339 144.848 381.616 131.406C377.217 125.891 370.708 119.551 365.955 113.774C361.028 107.786 356.424 101.22 350.958 95.7094C348.446 93.1774 344.972 91.5991 342.655 88.8896C339.64 85.3654 336.247 77.9596 332.583 75.6885C335.67 75.5085 338.09 77.7557 340.891 78.8218L341.095 78.3767C370.306 91.6591 398.71 117.535 416.519 143.598C423.932 154.446 434.017 171.171 437.658 183.731Z"/><path fill="#FEFEFE" transform="scale(2 2)" d="M215.654 206.985C226.548 206.135 240.731 206.988 252.001 206.98C276.842 206.962 279.064 202.625 279.053 230.081L279.044 342.646C297.402 342.567 304.684 338.528 304.758 359.714C304.781 366.319 306.548 375.707 297.992 377.638C293.233 378.712 279.365 378.099 273.658 378.086L234.115 378.08C221.479 378.088 208.292 382.402 208.254 366.055C208.197 342.003 205.985 342.484 231.846 342.646L231.846 239.982C226.459 240.001 210.402 242.44 208.674 235.199C207.959 232.204 207.953 216.829 208.429 213.531C209.051 209.221 211.38 207.633 215.654 206.985Z"/></svg>`

// iconButton builds a button showing an inline SVG. Rendering an SVG needs the
// librsvg pixbuf loader, which a GTK desktop normally has but is not
// guaranteed; if the texture cannot be made the button falls back to the given
// text so it is still usable.
func iconButton(svg, fallback, label, tooltip string) *gtk.Button {
	b := gtk.NewButton()
	b.SetTooltipText(tooltip)

	var icon *gtk.Image
	if tex, err := gdk.NewTextureFromBytes(glib.NewBytesWithGo([]byte(svg))); err == nil {
		icon = gtk.NewImageFromPaintable(tex)
		icon.SetPixelSize(18)
	}
	switch {
	case icon != nil && label != "":
		box := gtk.NewBox(gtk.OrientationHorizontal, 6)
		box.Append(icon)
		box.Append(gtk.NewLabel(label))
		b.SetChild(box)
	case icon != nil:
		b.SetChild(icon)
	case label != "":
		b.SetLabel(fallback + " " + label)
	default:
		b.SetLabel(fallback)
	}
	return b
}

// pieceSet is how one font draws the twelve pieces.
//
//	glyphs[fen] = {outline, solid}
//
// The outline glyph is the piece as drawn; the solid one is the same piece
// filled, used underneath a white piece so its body is white rather than
// showing the square through it. Black pieces need no underlay.
type pieceSet struct {
	family string
	glyphs map[rune][2]string
	// byBaseline is for dedicated chess fonts, where one glyph is exactly one
	// square: the em box is the square, so the glyph is placed on the square's
	// baseline rather than centred on its ink.
	byBaseline bool
	scale      float64 // font size as a fraction of the square
}

// Chess Alpha (Eric Bentzen) maps pieces onto ASCII, one letter per piece and
// colour, with no square painted behind them — which is what lets us keep our
// own brown squares. Mapping read off the font's own glyphs, not assumed.
var alphaSet = pieceSet{
	family: "Chess Alpha",
	glyphs: map[rune][2]string{
		'K': {"k", "l"}, 'Q': {"q", "w"}, 'R': {"r", "t"},
		'B': {"b", "n"}, 'N': {"h", "j"}, 'P': {"p", "o"},
		'k': {"l", ""}, 'q': {"w", ""}, 'r': {"t", ""},
		'b': {"n", ""}, 'n': {"j", ""}, 'p': {"o", ""},
	},
	byBaseline: true,
	// Chess Alpha draws a piece across its whole em, which is the square. A
	// little under that leaves air between the piece and the square's edges.
	scale: 0.86,
}

// The fallback: the Unicode chess range, which every desktop font of any size
// carries. Placed by ink extents, since these glyphs are text, not squares.
func unicodeSet(family string) pieceSet {
	return pieceSet{
		family: family,
		glyphs: map[rune][2]string{
			'K': {"\u2654", "\u265a"}, 'Q': {"\u2655", "\u265b"},
			'R': {"\u2656", "\u265c"}, 'B': {"\u2657", "\u265d"},
			'N': {"\u2658", "\u265e"}, 'P': {"\u2659", "\u265f"},
			'k': {"\u265a", ""}, 'q': {"\u265b", ""}, 'r': {"\u265c", ""},
			'b': {"\u265d", ""}, 'n': {"\u265e", ""}, 'p': {"\u265f", ""},
		},
		scale: 0.86,
	}
}

// pieces picks the nicest set this machine actually has. The font is NOT
// embedded in the binary — Cairo's Go binding only offers the toy font API,
// which resolves a family name through whatever font system the platform has,
// so the font has to be installed on the machine that runs the program.
//
// Detection is by metrics rather than by asking the font system, because that
// works the same on Linux and on Windows. A dedicated chess font draws one
// square per glyph: every advance is a full em and there is no descender. A
// text font quietly substituted in its place fails both tests. That check
// matters: with a chess font's ASCII mapping, a silent substitution would
// print the letters k, q, r on the squares instead of pieces.
var (
	piecesOnce sync.Once
	pieceCache pieceSet
)

func pieces(cr *cairo.Context) pieceSet {
	piecesOnce.Do(func() {
		isSquareFont := func(family string) bool {
			cr.SelectFontFace(family, cairo.FontSlantNormal, cairo.FontWeightNormal)
			cr.SetFontSize(100)
			return cr.TextExtents("k").XAdvance > 90 &&
				cr.TextExtents("q").XAdvance > 90 &&
				cr.FontExtents().Descent < 5
		}
		if isSquareFont(alphaSet.family) {
			pieceCache = alphaSet
			return
		}

		// Fall back to the Unicode chess range. Which families are likely to
		// carry it depends on the platform.
		families := []string{"Noto Sans Symbols2", "DejaVu Sans", "FreeSerif"}
		if runtime.GOOS == "windows" {
			families = []string{"Segoe UI Symbol", "Segoe UI Historic", "DejaVu Sans"}
		}
		// fc-list can confirm a family where fontconfig exists; where it does
		// not (Windows), take the first candidate and let the platform
		// substitute if need be.
		for _, f := range families {
			out, err := exec.Command("fc-list", f, "family").Output()
			if err != nil {
				pieceCache = unicodeSet(families[0])
				return
			}
			if strings.TrimSpace(string(out)) != "" {
				pieceCache = unicodeSet(f)
				return
			}
		}
		pieceCache = unicodeSet(families[0])
	})
	return pieceCache
}

func sanFor(fen string, moves []string) map[string]string {
	out := map[string]string{}
	for _, m := range moves {
		out[m] = m
	}
	fenOpt, err := chess.FEN(fen)
	if err != nil {
		return out
	}
	game := chess.NewGame(fenOpt)
	pos := game.Position()
	uci := chess.UCINotation{}
	alg := chess.AlgebraicNotation{}
	for _, m := range moves {
		mv, err := uci.Decode(pos, m)
		if err != nil {
			continue
		}
		out[m] = alg.Encode(pos, mv)
	}
	return out
}

// ---------------------------------------------------------------------------
// running the engine
// ---------------------------------------------------------------------------

type runConfig struct {
	enginePath string
	fen        string
	multipv    int
	maxSeconds float64
	maxDepth   int // 0 = no depth cap
	hash       string
	threads    int

	// Called once the engine has stopped, with the whole conversation in the
	// order it happened. Used to write the .log beside the graph.
	onDone func(transcript []string)
}

type runner struct {
	cmd  *exec.Cmd
	stop chan struct{}
	once sync.Once
}

func (r *runner) halt() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.stop) })
}

// start launches the engine in its own goroutine and feeds results into st.
// redraw is called (from the GTK thread) whenever something changed.
func start(cfg runConfig, st *search, redraw func()) (*runner, error) {
	cmd := exec.Command(cfg.enginePath)
	cmd.Dir = filepath.Dir(cfg.enginePath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	r := &runner{cmd: cmd, stop: make(chan struct{})}
	lines := make(chan string, 256)

	// Everything said in either direction, for the .log written afterwards.
	// The reader runs in its own goroutine, hence the mutex.
	var tmu sync.Mutex
	var transcript []string
	record := func(prefix, line string) {
		tmu.Lock()
		transcript = append(transcript, prefix+line)
		tmu.Unlock()
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			record("< ", sc.Text())
			lines <- sc.Text()
		}
		close(lines)
	}()

	send := func(s string) {
		record("> ", s)
		io.WriteString(stdin, s+"\n")
	}

	go func() {
		defer func() {
			send("quit")
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
			}
			if cfg.onDone != nil {
				tmu.Lock()
				snapshot := append([]string(nil), transcript...)
				tmu.Unlock()
				cfg.onDone(snapshot)
			}
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
			glib.IdleAdd(redraw)
		}()

		setStatus := func(msg string) {
			st.mu.Lock()
			st.status = msg
			st.mu.Unlock()
			glib.IdleAdd(redraw)
		}

		// Handshake, collecting the option names so we can warn rather than
		// blindly set something the engine does not have.
		options := map[string]bool{}
		send("uci")
		deadline := time.After(15 * time.Second)
		gotUCIOK := false
	handshake:
		for !gotUCIOK {
			select {
			case line, ok := <-lines:
				if !ok {
					setStatus("engine closed its output during handshake")
					return
				}
				if strings.HasPrefix(line, "option name ") {
					rest := line[len("option name "):]
					if i := strings.Index(rest, " type "); i > 0 {
						options[rest[:i]] = true
					}
				}
				if strings.HasPrefix(line, "id name ") {
					st.mu.Lock()
					st.engineID = strings.TrimSpace(line[len("id name "):])
					st.mu.Unlock()
				}
				if strings.TrimSpace(line) == "uciok" {
					gotUCIOK = true
					break handshake
				}
			case <-deadline:
				setStatus("no 'uciok' within 15s — is this really a UCI engine?")
				return
			case <-r.stop:
				return
			}
		}

		var missing []string
		setOpt := func(name, value string) {
			if !options[name] {
				missing = append(missing, name)
				return
			}
			send(fmt.Sprintf("setoption name %s value %s", name, value))
		}
		setOpt("MultiPV", strconv.Itoa(cfg.multipv))
		if cfg.hash != "" {
			setOpt("Hash", cfg.hash)
		}
		if cfg.threads > 0 {
			setOpt("Threads", strconv.Itoa(cfg.threads))
		}
		send("isready")

		// Wait for readyok, but do not hang forever on a sloppy engine.
		ready := time.After(20 * time.Second)
	waitReady:
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					setStatus("engine closed its output before readyok")
					return
				}
				if strings.TrimSpace(line) == "readyok" {
					break waitReady
				}
			case <-ready:
				break waitReady
			case <-r.stop:
				return
			}
		}

		send("position fen " + cfg.fen)
		// 'go depth N' is standard, but it gives no wall-clock bound: with
		// MultiPV one iteration can run for minutes. So the clock is enforced
		// here and 'go infinite' keeps it to a single stop path.
		send("go infinite")

		begin := time.Now()
		st.mu.Lock()
		st.running = true
		st.started = begin
		st.mu.Unlock()

		if len(missing) > 0 {
			setStatus("searching — engine has no " + strings.Join(missing, ", ") +
				" option, continuing without it")
		} else {
			setStatus("searching…")
		}

		clock := time.NewTicker(100 * time.Millisecond)
		defer clock.Stop()
		stopped := false
		reason := ""
		grace := time.After(24 * time.Hour) // replaced once we send 'stop'

		for {
			select {
			case line, ok := <-lines:
				if !ok {
					st.mu.Lock()
					st.stoppedAt = time.Since(begin)
					st.mu.Unlock()
					setStatus("Done: engine output ended — " + reason)
					return
				}
				if strings.HasPrefix(line, "bestmove") {
					f := strings.Fields(line)
					st.mu.Lock()
					if len(f) > 1 {
						st.best = f[1]
					}
					st.stoppedAt = time.Since(begin)
					// Named here while the lock is held; sanOrUCI falls back
					// to the raw UCI move if the position never produced it.
					best := ""
					if st.best != "" {
						best = st.sanOrUCI(st.best)
					}
					st.mu.Unlock()
					if reason == "" {
						// The engine finished on its own, inside the caps, so
						// its choice is worth naming.
						reason = "engine returned bestmove"
						if best != "" {
							reason += " " + best
						}
					}
					setStatus("Done: " + reason)
					return
				}
				if stopped {
					// Anything after 'stop' tends to be a partial re-dump of the
					// interrupted iteration, often with placeholder scores.
					continue
				}
				in := parseInfo(line)
				if !in.ok {
					continue
				}
				if in.isMate && in.mate == 0 {
					continue // never a real evaluation
				}
				cp := in.cp
				if in.isMate {
					cp = 10
					if in.mate < 0 {
						cp = -10
					}
				}
				st.mu.Lock()
				st.note(in.move, in.depth, in.multipv, cp, in.mate, in.isMate,
					in.timeMS, int(time.Since(begin).Milliseconds()))
				if _, ok := st.san[in.move]; !ok {
					st.san = sanFor(cfg.fen, st.seen)
				}
				full := st.lastComplete()
				st.mu.Unlock()
				glib.IdleAdd(redraw)

				if cfg.maxDepth > 0 && full >= cfg.maxDepth && !stopped {
					stopped, reason = true, fmt.Sprintf("depth limit reached (%d)", cfg.maxDepth)
					send("stop")
					grace = time.After(10 * time.Second)
					setStatus("depth limit reached, stopping…")
				}

			case <-clock.C:
				if !stopped && time.Since(begin).Seconds() >= cfg.maxSeconds {
					stopped, reason = true, fmt.Sprintf("time limit reached (%.0fs)", cfg.maxSeconds)
					send("stop")
					grace = time.After(10 * time.Second)
					setStatus("time limit reached, stopping…")
				}
				glib.IdleAdd(redraw)

			case <-grace:
				st.mu.Lock()
				st.stoppedAt = time.Since(begin)
				st.mu.Unlock()
				setStatus("engine did not answer 'stop' — terminated")
				cmd.Process.Kill()
				return

			case <-r.stop:
				if !stopped {
					stopped, reason = true, "stopped by you"
					send("stop")
					grace = time.After(10 * time.Second)
					setStatus("stopping…")
				}
			}
		}
	}()

	return r, nil
}

// ---------------------------------------------------------------------------
// drawing
// ---------------------------------------------------------------------------

type rgb struct{ r, g, b float64 }

// A line keeps the colour of the rank it currently holds, so a move that
// climbs the list changes colour as it goes. The first nine run warm to cool,
// which is the order the ranks are read in.
var palette = []rgb{
	{0.839, 0.153, 0.157}, // 1  red
	{1.000, 0.498, 0.055}, // 2  orange
	{0.737, 0.741, 0.133}, // 3  dark yellow
	{0.173, 0.627, 0.173}, // 4  green
	{0.122, 0.467, 0.706}, // 5  dark blue
	{0.580, 0.404, 0.741}, // 6  dark pink
	{0.549, 0.337, 0.294}, // 7  brown
	{0.090, 0.745, 0.812}, // 8  light blue
	{0.890, 0.467, 0.761}, // 9  light pink
	{0.498, 0.498, 0.498}, // 10 grey
	{0.224, 0.231, 0.475}, // 11 indigo
	{0.710, 0.396, 0.114}, // 12 burnt orange
	{0.322, 0.329, 0.639}, // 13 slate blue
	{0.549, 0.635, 0.322}, // 14 olive
	{0.678, 0.286, 0.290}, // 15 dark red
	{0.769, 0.612, 0.580}, // 16 tan
	{0.420, 0.431, 0.812}, // 17 periwinkle
	{0.906, 0.588, 0.612}, // 18 rose
}

// Ties are fanned apart by a fixed number of pixels rather than by an amount
// of evaluation, so the separation does not change with the vertical scale.
const nudgePx = 6.0

// startupSize returns the window size to open with: 1500 wide at most, and
// never wider or taller than the monitor it appears on, leaving a margin so
// the frame and the panel have room. If the monitor cannot be read — no
// display yet, or a backend that does not report one — the fallback is a size
// that fits a 1366-wide laptop screen.
func startupSize() (int, int) {
	const maxW, maxH = 1500, 950

	_, _, mw, mh, ok := monitorGeometry()
	if !ok {
		return 1280, 800
	}

	w := mw - 80
	if w > maxW {
		w = maxW
	}
	h := mh - 120
	if h > maxH {
		h = maxH
	}
	if w < 900 {
		w = 900
	}
	if h < 600 {
		h = 600
	}
	return w, h
}

// monitorGeometry returns the primary monitor's rectangle, or false when it
// cannot be read.
func monitorGeometry() (x, y, w, h int, ok bool) {
	display := gdk.DisplayGetDefault()
	if display == nil {
		return 0, 0, 0, 0, false
	}
	monitors := display.Monitors()
	if monitors == nil || monitors.NItems() == 0 {
		return 0, 0, 0, 0, false
	}
	obj := monitors.Item(0)
	if obj == nil {
		return 0, 0, 0, 0, false
	}
	// gotk4's own wrapper for a monitor is exactly this: the GObject placed in
	// the typed struct (see wrapMonitor in the binding).
	geo := (&gdk.Monitor{Object: obj}).Geometry()
	if geo == nil || geo.Width() <= 0 || geo.Height() <= 0 {
		return 0, 0, 0, 0, false
	}
	return geo.X(), geo.Y(), geo.Width(), geo.Height(), true
}

// winTitle is the window's title.
const winTitle = "RBevalgraph — chess position analyser with live MultiPV " +
	"evaluation graph"

// pngScale is how much larger than the canvas a saved image is drawn. The
// layout is identical; only the resolution goes up.
const pngScale = 2

// cornerRadius is how far from a vertex a line starts bending, in pixels.
const cornerRadius = 10.0

// quantile returns the q-quantile of an already sorted slice.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// config is what survives between runs. It lives next to the binary so the
// program stays self-contained; if that directory is not writable the working
// directory is used instead.
type config struct {
	Engine        string `yaml:"engine"`
	FEN           string `yaml:"fen"`
	MultiPV       int    `yaml:"multipv"`
	MaxSeconds    int    `yaml:"max_seconds"`
	MaxDepth      int    `yaml:"max_depth"`
	HashMB        int    `yaml:"hash_mb"`
	HashChoice    string `yaml:"hash_choice"`
	Threads       int    `yaml:"threads"`
	LastEngineDir string `yaml:"last_save_dir_engine"`
	LastPNGDir    string `yaml:"last_save_dir_png"`
}

func configPath() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if f, err := os.OpenFile(filepath.Join(dir, ".rbevalgraph-write-test"),
			os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			name := f.Name()
			f.Close()
			os.Remove(name)
			return filepath.Join(dir, "config.yaml")
		}
	}
	return "config.yaml"
}

// defaultConfig is what a first run looks like: no engine, no position,
// one PV line, a minute of search, no depth limit, one thread. HashMB is left
// at zero on purpose — it means "whatever the engine says", and until an
// engine has been loaded the Hash field shows --- rather than a made-up number.
func defaultConfig() config {
	return config{MultiPV: 1, MaxSeconds: 60, MaxDepth: 0, HashMB: 0, Threads: 1}
}

func loadConfig() config {
	c := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		// A corrupt file should not stop the program; the defaults stand and
		// the next save overwrites it.
		return defaultConfig()
	}
	return c
}

func (c config) save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o644)
}

// resetTimer restarts t, draining it first if it had already fired.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// xboardResult is what a CECP ("XBoard") engine told us about itself.
type xboardResult struct {
	isXBoard bool
	name     string        // from feature myname="…"
	raw      []string      // what it said, verbatim
	took     time.Duration // how long it needed to answer
}

// probeXBoard is the second guess when a binary does not answer "uci". CECP
// engines reply to "protover 2" with a run of "feature …" lines, one of which
// carries myname. An engine that answers neither protocol simply comes back
// with isXBoard false.
func probeXBoard(path string, notify func(string)) xboardResult {
	start := time.Now()
	var res xboardResult
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return res
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res
	}
	if err := cmd.Start(); err != nil {
		return res
	}
	defer func() {
		io.WriteString(stdin, "quit\n")
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
		}
	}()

	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<16)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	if _, err := io.WriteString(stdin, "xboard\nprotover 2\n"); err != nil {
		return res
	}
	idle := time.NewTimer(probeIdle)
	defer idle.Stop()
	overall := time.After(probeMax)
	toldUser := false
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return res
			}
			resetTimer(idle, probeIdle)
			line = strings.TrimRight(line, " \t\r")
			if line != "" {
				res.raw = append(res.raw, line)
			}
			if strings.HasPrefix(line, "feature ") {
				res.isXBoard = true
				if n := featureString(line, "myname"); n != "" {
					res.name = n
				}
				// "done=0" is the engine saying it needs time; the protocol
				// says the GUI then waits for "done=1" however long it takes.
				// We wait, but not past the ceiling.
				if strings.Contains(line, "done=0") && !toldUser {
					toldUser = true
					// It has told us it will be quiet for a while, so the
					// silence clock no longer applies — only the ceiling.
					resetTimer(idle, probeMax)
					if notify != nil {
						notify("initialising")
					}
				}
				if strings.Contains(line, "done=1") {
					res.took = time.Since(start)
					return res
				}
			}
		case <-idle.C:
			res.took = time.Since(start)
			return res
		case <-overall:
			res.took = time.Since(start)
			return res
		}
	}
}

// featureString pulls key="value" out of a CECP feature line.
func featureString(line, key string) string {
	i := strings.Index(line, key+"=\"")
	if i < 0 {
		return ""
	}
	rest := line[i+len(key)+2:]
	if j := strings.Index(rest, "\""); j >= 0 {
		return rest[:j]
	}
	return ""
}

// checkFEN returns an empty string when the FEN is usable, or a sentence
// explaining what is wrong with it. A position the engine cannot search — one
// with no legal move — is rejected here rather than after a puzzling silence.
func checkFEN(fen string) string {
	fenOpt, err := chess.FEN(fen)
	if err != nil {
		return "that FEN is not valid: " + err.Error()
	}
	game := chess.NewGame(fenOpt)
	if len(game.ValidMoves()) == 0 {
		return "that position has no legal moves, so there is nothing to search"
	}
	return ""
}

// strokePolyline draws a run of points with its corners rounded off, so the
// lines flow instead of spiking. The rounding never leaves the corridor of the
// original polyline: the radius is clamped to half of the shorter neighbouring
// segment, so no curve can overshoot into an evaluation the engine never gave.
func strokePolyline(cr *cairo.Context, pts [][2]float64, radius float64) {
	if len(pts) == 0 {
		return
	}
	if len(pts) < 3 {
		cr.MoveTo(pts[0][0], pts[0][1])
		for _, p := range pts[1:] {
			cr.LineTo(p[0], p[1])
		}
		cr.Stroke()
		return
	}
	dist := func(a, b [2]float64) float64 {
		return math.Hypot(b[0]-a[0], b[1]-a[1])
	}
	along := func(from, to [2]float64, r float64) [2]float64 {
		d := dist(from, to)
		if d == 0 {
			return from
		}
		return [2]float64{from[0] + (to[0]-from[0])*r/d, from[1] + (to[1]-from[1])*r/d}
	}

	cr.MoveTo(pts[0][0], pts[0][1])
	for i := 1; i < len(pts)-1; i++ {
		prev, cur, next := pts[i-1], pts[i], pts[i+1]
		r := math.Min(radius, math.Min(dist(prev, cur), dist(cur, next))/2)
		a := along(cur, prev, r)
		b := along(cur, next, r)
		cr.LineTo(a[0], a[1])
		// quadratic through the corner, written as the equivalent cubic
		cr.CurveTo(
			a[0]+2.0/3.0*(cur[0]-a[0]), a[1]+2.0/3.0*(cur[1]-a[1]),
			b[0]+2.0/3.0*(cur[0]-b[0]), b[1]+2.0/3.0*(cur[1]-b[1]),
			b[0], b[1])
	}
	last := pts[len(pts)-1]
	cr.LineTo(last[0], last[1])
	cr.Stroke()
}

// drawBoard paints an 8x8 diagram with the usual light/dark brown squares.
// Pieces are Unicode glyphs: the filled shape for black, and for white the
// filled shape in white with the hollow shape laid over it as an outline.
// drawBoard paints the position. flipped puts Black at the bottom. The rank
// numbers go in a gutter to the right of the squares and the file letters
// below them; gutter is how much room those take, so the caller can keep the
// board plus its coordinates inside a known width.
func drawBoard(cr *cairo.Context, x, y, size float64, board [8][8]rune,
	flipped bool, gutter float64) {
	empty := true
	for r := 0; r < 8 && empty; r++ {
		for f := 0; f < 8; f++ {
			if board[r][f] != 0 {
				empty = false
				break
			}
		}
	}
	if empty {
		return
	}

	sq := size / 8
	// at(row, col) says which board square is drawn in that screen cell
	at := func(row, col int) (int, int) {
		if flipped {
			return 7 - row, 7 - col
		}
		return row, col
	}
	for r := 0; r < 8; r++ {
		for f := 0; f < 8; f++ {
			br, bf := at(r, f)
			if (br+bf)%2 == 0 {
				cr.SetSourceRGB(0.941, 0.851, 0.710) // light brown
			} else {
				cr.SetSourceRGB(0.710, 0.533, 0.388) // dark brown
			}
			cr.Rectangle(x+float64(f)*sq, y+float64(r)*sq, sq, sq)
			cr.Fill()
		}
	}
	cr.SetSourceRGBA(0, 0, 0, 0.55)
	cr.SetLineWidth(1)
	cr.Rectangle(x, y, sq*8, sq*8)
	cr.Stroke()

	set := pieces(cr)
	cr.SelectFontFace(set.family, cairo.FontSlantNormal, cairo.FontWeightNormal)
	cr.SetFontSize(sq * set.scale)
	ascent := cr.FontExtents().Ascent
	for r := 0; r < 8; r++ {
		for f := 0; f < 8; f++ {
			br, bf := at(r, f)
			g, ok := set.glyphs[board[br][bf]]
			if !ok {
				continue
			}
			var gx, gy float64
			if set.byBaseline {
				// The glyph box is one em; centre that box in the square so
				// the margin is even on all four sides.
				pad := (sq - sq*set.scale) / 2
				gx = x + float64(f)*sq + pad
				gy = y + float64(r)*sq + pad + ascent
			} else {
				ext := cr.TextExtents(g[0])
				gx = x + float64(f)*sq + (sq-ext.Width)/2 - ext.XBearing
				gy = y + float64(r)*sq + (sq-ext.Height)/2 - ext.YBearing
			}
			if g[1] != "" { // a white piece: white body, then its outline
				cr.SetSourceRGB(1, 1, 1)
				cr.MoveTo(gx, gy)
				cr.ShowText(g[1])
			}
			cr.SetSourceRGB(0.08, 0.08, 0.08)
			cr.MoveTo(gx, gy)
			cr.ShowText(g[0])
		}
	}

	// Coordinates: ranks in the gutter on the right, files underneath. Small,
	// grey, centred on their row or column.
	if gutter > 0 {
		cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, cairo.FontWeightNormal)
		cr.SetFontSize(math.Min(10, gutter*0.72))
		cr.SetSourceRGB(0.45, 0.45, 0.45)
		for r := 0; r < 8; r++ {
			br, _ := at(r, 0)
			label := strconv.Itoa(8 - br)
			ext := cr.TextExtents(label)
			cr.MoveTo(x+size+(gutter-ext.Width)/2-ext.XBearing,
				y+float64(r)*sq+(sq+ext.Height)/2)
			cr.ShowText(label)
		}
		for f := 0; f < 8; f++ {
			_, bf := at(0, f)
			label := string(rune('a' + bf))
			ext := cr.TextExtents(label)
			cr.MoveTo(x+float64(f)*sq+(sq-ext.Width)/2-ext.XBearing,
				y+size+(gutter+ext.Height)/2)
			cr.ShowText(label)
		}
	}
	cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, cairo.FontWeightNormal)
}

// sanitize keeps a file name portable: letters, digits, dot, dash and
// underscore survive, every run of anything else becomes a single dash.
func sanitize(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

// writeSearchLog saves the whole UCI conversation next to where the graph
// would be saved, under the same name with a .log extension. Engines
// occasionally produce odd output; this is what you read afterwards to see
// exactly what was asked and what came back.
func writeSearchLog(path string, header []string, transcript []string) error {
	var b strings.Builder
	for _, l := range header {
		b.WriteString("# " + l + "\n")
	}
	b.WriteString("#\n# > is what RBevalgraph sent, < is what the engine said\n#\n")
	for _, l := range transcript {
		b.WriteString(l + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// suggestedPNGName builds
// eval-graph-<engine>-maxdep<n>-maxsec<n>-mpv<n>.png
func (s *search) suggestedPNGName() string {
	name := sanitize(s.engineID)
	if name == "" {
		name = "engine"
	}
	return fmt.Sprintf("eval-graph-%s-maxdep%d-maxsec%.0f-mpv%d.png",
		name, s.maxDepth, s.maxSecs, s.multipv)
}

func draw(cr *cairo.Context, width, height int, st *search) {
	st.mu.Lock()
	defer st.mu.Unlock()

	W, H := float64(width), float64(height)
	cr.SetSourceRGB(1, 1, 1)
	cr.Rectangle(0, 0, W, H)
	cr.Fill()
	cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, cairo.FontWeightNormal)

	face := func(size float64, bold bool, c rgb) {
		w := cairo.FontWeightNormal
		if bold {
			w = cairo.FontWeightBold
		}
		cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, w)
		cr.SetFontSize(size)
		cr.SetSourceRGB(c.r, c.g, c.b)
	}
	text := func(x, y, size float64, bold bool, c rgb, s string) {
		face(size, bold, c)
		cr.MoveTo(x, y)
		cr.ShowText(s)
	}
	textRight := func(x, y, size float64, bold bool, c rgb, s string) {
		face(size, bold, c)
		ext := cr.TextExtents(s)
		cr.MoveTo(x-ext.Width, y)
		cr.ShowText(s)
	}
	textCentre := func(x, y, size float64, bold bool, c rgb, s string) {
		face(size, bold, c)
		ext := cr.TextExtents(s)
		cr.MoveTo(x-ext.Width/2, y)
		cr.ShowText(s)
	}
	black := rgb{0.1, 0.1, 0.1}
	grey := rgb{0.4, 0.4, 0.4}

	// Geometry first: the diagram now sits at the top of the right-hand
	// column, so the header lines have to stop before it.
	keyW := 260.0
	left, right := 74.0, W-keyW-24
	top, bottom := 104.0, H-46
	headerLimit := right + 8
	if st.running {
		headerLimit = right - 110 // room for the countdown
	}

	// The diagram depends only on the position, so it is drawn as soon as
	// there is a valid FEN — before any search has run.
	kx := right + 20
	listTop := top + 6
	boardX, boardY, boardSq := 0.0, 0.0, 0.0
	if st.fen != "" {
		// The squares span the full width of the move list, so the board's
		// right edge lines up with the evaluation figures; the rank numbers
		// then sit outside that, in the window's right margin.
		const gutter = 14.0
		size := keyW - 24
		boardTop := 14.0
		drawBoard(cr, kx, boardTop, size, st.board, st.flipped, gutter)
		boardX, boardY, boardSq = kx, boardTop, size/8
		side := "White to move"
		if st.sideToMove == 'b' {
			side = "Black to move"
		}
		textCentre(kx+size/2, boardTop+size+gutter+18, 11.5, false, grey, side)
		if y := boardTop + size + gutter + 40; y > listTop {
			listTop = y
		}
	}

	// While an engine is being identified there is nothing to say about a
	// search, and half-written headings would only flicker past. The position
	// stays on screen, and so does the status line — it is the one thing that
	// does apply, saying which protocol is being tried and for how long.
	if st.probing {
		text(20, H-10, 11.5, false, grey, st.status)
		return
	}

	// ---- header ------------------------------------------------------------
	title := st.engineID
	if title == "" {
		title = "[ no UCI engine selected ]"
	}
	text(20, 28, 15, true, black, title)

	// Without a loaded engine — none chosen yet, or the chosen one rejected —
	// nothing else on the canvas applies: no units line, no FEN line, no
	// search to report on.
	if st.engineID == "" {
		return
	}
	// The line must stop before the diagram, like the FEN line below it; in
	// a very narrow window it is cut with an ellipsis. (The scale note that
	// used to follow it now stands upright beside the eval axis.)
	drawSubtitle := func() {
		line := "eval values in 'pawns' (cp / 100), higher is better " +
			"(values from engine's point of view)"
		limit := right + 8
		face(11.5, false, grey)
		if 20+cr.TextExtents(line).Width > limit {
			for len(line) > 12 && 20+cr.TextExtents(line+"…").Width > limit {
				line = line[:len(line)-1] // ASCII only here, so byte-safe
			}
			line += "…"
		}
		text(20, 48, 11.5, false, grey, line)
	}
	if st.fen != "" {
		cr.SelectFontFace("monospace", cairo.FontSlantNormal, cairo.FontWeightNormal)
		cr.SetFontSize(10.5)
		line := "FEN  " + st.fen
		if st.meta != "" {
			line += "   ·   " + st.meta
		}
		for len(line) > 12 && 20+cr.TextExtents(line+"…").Width > headerLimit {
			line = line[:len(line)-1]
		}
		if !strings.HasSuffix(line, st.meta) && st.meta != "" {
			line += "…"
		}
		text(20, 70, 10.5, false, grey, line)
		cr.SelectFontFace("sans-serif", cairo.FontSlantNormal, cairo.FontWeightNormal)
	}
	// Detection returns above, so anything shown here is a settled message.
	text(20, H-10, 11.5, false, black, st.status)

	// Countdown, sitting directly above the "< elapsed seconds >" caption at
	// the right edge of the plot — not in the column, where the diagram is.
	switch {
	case st.running && !st.started.IsZero():
		remain := st.maxSecs - time.Since(st.started).Seconds()
		if remain < 0 {
			remain = 0
		}
		c := rgb{0.1, 0.35, 0.1}
		if remain <= 10 {
			c = rgb{0.72, 0.15, 0.15}
		}
		textRight(right, top-44, 15, true, c, fmt.Sprintf("%.0fs left", remain))
	case st.running:
		textRight(right, top-44, 12, false, grey, "starting…")
	}

	// ---- plot area ---------------------------------------------------------
	if right < left+120 || bottom < top+80 {
		drawSubtitle()
		return // window too small to draw anything sensible
	}

	ds := st.depths()
	if len(ds) == 0 {
		drawSubtitle()
		text(left+10, top+30, 12, false, grey,
			"Browse engine and enter FEN, set your options and press "+
				"`Generate eval graph`")
		return
	}
	minDepth := ds[0]
	maxDepth := ds[len(ds)-1]
	axisMax := maxDepth
	if st.maxDepth > 0 && st.maxDepth > axisMax {
		axisMax = st.maxDepth
	}
	if axisMax < minDepth+7 {
		axisMax = minDepth + 7
	}

	// Values are drawn exactly as the engine reports them: from the engine's
	// point of view, the side to move. There is no White's-point-of-view
	// conversion any more; it was a checkbox once and has been removed.

	order, topN := st.ordered()

	// --- vertical scale -----------------------------------------------------
	// Engine evaluations cluster. Most candidates sit within a few centipawns
	// of each other while one line wanders far off, and on a plain linear axis
	// the cluster collapses into a stripe. So the axis is centred on the
	// cluster and compressed outside it: linear within one robust spread of
	// the centre, logarithmic beyond that.
	var all []float64
	for _, m := range order {
		for d := range st.evals[m] {
			all = append(all, st.evals[m][d])
		}
	}
	sort.Float64s(all)
	centre := quantile(all, 0.5)
	devs := make([]float64, len(all))
	for i, v := range all {
		devs[i] = math.Abs(v - centre)
	}
	sort.Float64s(devs)
	spread := quantile(devs, 0.75)
	if spread < 0.02 {
		spread = 0.02 // everything identical: keep a sane linear window
	}

	tOf := func(v float64) float64 {
		d := v - centre
		a := math.Abs(d)
		t := a / spread
		if a > spread {
			t = 1 + math.Log1p((a-spread)/spread)
		}
		return math.Copysign(t, d)
	}
	vOf := func(t float64) float64 {
		a := math.Abs(t)
		d := a * spread
		if a > 1 {
			d = spread * math.Exp(a-1)
		}
		return centre + math.Copysign(d, t)
	}

	tMax := 1.0
	for _, v := range all {
		if t := math.Abs(tOf(v)); t > tMax {
			tMax = t
		}
	}
	mid := (top + bottom) / 2
	half := (bottom - top) / 2
	coreHalf := half
	if tMax > 1 {
		coreHalf = half * 0.62 // the cluster keeps the middle ~62% of the height
	}
	tailHeight := half - coreHalf

	// The cluster centre lands in the vertical middle of the plot.
	py := func(v float64) float64 {
		t := tOf(v)
		a := math.Abs(t)
		off := a * coreHalf
		if a > 1 && tailHeight > 0 {
			off = coreHalf + (a-1)/(tMax-1)*tailHeight
		}
		if t < 0 {
			return mid + off
		}
		return mid - off
	}
	// the same mapping backwards, so the grid can be labelled with real values
	vAt := func(y float64) float64 {
		off := mid - y
		a := math.Abs(off)
		t := a / coreHalf
		if a > coreHalf && tailHeight > 0 {
			t = 1 + (a-coreHalf)/tailHeight*(tMax-1)
		}
		return vOf(math.Copysign(t, off))
	}

	// Fan tied values apart in pixels, stable per tie group.
	shiftPx := map[string]map[int]float64{}
	for _, m := range order {
		shiftPx[m] = map[int]float64{}
	}
	for _, d := range ds {
		groups := map[float64][]string{}
		var keys []float64
		for _, m := range order {
			if v, ok := st.evals[m][d]; ok {
				k := math.Round(v*10000) / 10000
				if _, seen := groups[k]; !seen {
					keys = append(keys, k)
				}
				groups[k] = append(groups[k], m)
			}
		}
		for _, k := range keys {
			g := groups[k]
			for i, m := range g {
				shiftPx[m][d] = (float64(i) - float64(len(g)-1)/2) * nudgePx
			}
		}
	}
	// yv is already in pixels.
	yv := func(m string, d int) float64 {
		return py(st.evals[m][d]) + shiftPx[m][d]
	}

	px := func(d int) float64 {
		if axisMax <= minDepth {
			return left
		}
		return left + (right-left)*float64(d-minDepth)/float64(axisMax-minDepth)
	}

	// ---- grid and axes -----------------------------------------------------
	cr.SetLineWidth(1)
	steps := 8
	labelW := 0.0 // widest eval label, so the scale note can stand beside them
	for i := 0; i <= steps; i++ {
		y := top + (bottom-top)*float64(i)/float64(steps)
		cr.SetSourceRGBA(0, 0, 0, 0.10)
		cr.MoveTo(left, y)
		cr.LineTo(right, y)
		cr.Stroke()
		label := fmt.Sprintf("%+.2f", vAt(y))
		face(10, false, grey)
		if w := cr.TextExtents(label).Width; w > labelW {
			labelW = w
		}
		textRight(left-8, y+4, 10, false, grey, label)
	}
	tick := 1
	for (axisMax-minDepth+1)/tick > 26 {
		tick++
	}
	for d := minDepth; d <= axisMax; d++ {
		if d%tick != 0 && d != minDepth {
			continue
		}
		x := px(d)
		cr.SetSourceRGBA(0, 0, 0, 0.08)
		cr.MoveTo(x, top)
		cr.LineTo(x, bottom)
		cr.Stroke()
		textCentre(x, bottom+16, 10, false, grey, strconv.Itoa(d))
	}

	// Elapsed time at every depth that reported one — not just the depths that
	// get an axis tick — skipping a label only where it would collide with the
	// one before it.
	face(9, false, grey)
	lastRight := -1e9
	for _, d := range ds {
		ms, ok := st.times[d]
		if !ok {
			continue
		}
		secs := float64(ms) / 1000
		label := fmt.Sprintf("%.1f", secs)
		if secs >= 10 {
			label = fmt.Sprintf("%.0f", secs)
		}
		w := cr.TextExtents(label).Width
		x := px(d)
		if x-w/2 < lastRight+4 {
			continue
		}
		textCentre(x, top-8, 9, false, grey, label)
		lastRight = x + w/2
	}
	textRight(right, top-24, 10, false, grey, "< elapsed seconds >")
	textRight(right, bottom+34, 11, false, grey, "< search depth (ply) >")
	drawSubtitle()
	// The scale note stands upright at the left of the eval labels, read
	// bottom to top and centred on the plot height. It only appears when the
	// scale is actually compressed. Rotated by -90 degrees, the glyphs rise to
	// the left of the baseline, so the baseline sits just left of the widest
	// label.
	if tMax > 1.4 {
		note := "eval scale centred on the line cluster, evals compressed outside it"
		face(10, false, grey)
		if span := bottom - top; cr.TextExtents(note).Width > span {
			for len(note) > 12 && cr.TextExtents(note+"…").Width > span {
				note = note[:len(note)-1] // ASCII only here, so byte-safe
			}
			note += "…"
		}
		x := math.Max(left-8-labelW-6, 12)
		cr.Save()
		cr.Translate(x, (top+bottom)/2)
		cr.Rotate(-math.Pi / 2)
		textCentre(0, 0, 10, false, grey, note)
		cr.Restore()
	}

	// zero line, when zero is on screen at all
	if y0 := py(0); y0 >= top && y0 <= bottom {
		cr.SetSourceRGBA(0, 0, 0, 0.45)
		cr.MoveTo(left, y0)
		cr.LineTo(right, y0)
		cr.Stroke()
	}

	// ---- the curves --------------------------------------------------------
	for i, m := range order {
		isTop := i < topN
		c := palette[i%len(palette)]
		var dd []int
		for d := range st.evals[m] {
			dd = append(dd, d)
		}
		sort.Ints(dd)
		lw := 1.3
		if isTop {
			lw = 2.4
		}

		cr.SetSourceRGB(c.r, c.g, c.b)
		cr.SetLineWidth(lw)
		cr.SetLineJoin(cairo.LineJoinRound)
		var chunk [][2]float64
		for j, d := range dd {
			if j > 0 && dd[j-1] != d-1 {
				strokePolyline(cr, chunk, cornerRadius)
				chunk = nil
			}
			chunk = append(chunk, [2]float64{px(d), yv(m, d)})
		}
		strokePolyline(cr, chunk, cornerRadius)

		for _, d := range dd {
			r := 2.8
			if isTop {
				r = 4.0
			}
			cr.SetSourceRGB(c.r, c.g, c.b)
			cr.Arc(px(d), yv(m, d), r/2+1, 0, 2*math.Pi)
			cr.Fill()
		}
		if !isTop && len(dd) > 0 {
			d := dd[len(dd)-1]
			text(px(d)+6, yv(m, d)+3, 9, false, c, st.sanOrUCI(m))
		}
	}

	// bestmove marker; the key column names it, so no caption here
	if st.best != "" {
		if ev, ok := st.evals[st.best]; ok && len(ev) > 0 {
			d := lastDepth(ev)
			c := palette[indexOf(order, st.best)%len(palette)]
			x, y := px(d), yv(st.best, d)
			cr.SetSourceRGB(c.r, c.g, c.b)
			cr.Arc(x, y, 7, 0, 2*math.Pi)
			cr.Fill()
			cr.SetSourceRGB(0, 0, 0)
			cr.SetLineWidth(1.2)
			cr.Arc(x, y, 7, 0, 2*math.Pi)
			cr.Stroke()
		}
	}

	cr.SetSourceRGBA(0, 0, 0, 0.45)
	cr.SetLineWidth(0.9)
	cr.MoveTo(px(maxDepth), top)
	cr.LineTo(px(maxDepth), bottom)
	cr.Stroke()

	// ---- key column --------------------------------------------------------
	// The diagram was drawn above; the list starts below it.
	ky := listTop

	hovered := ""
	for i, m := range order {
		c := palette[i%len(palette)]
		isTop := i < topN
		lw := 1.3
		size := 10.0
		if isTop {
			lw, size = 2.4, 12.0
		}

		// The row's own strip, used both for the highlight and for telling
		// whether the pointer is on it.
		rowTop, rowBottom := ky-14.0, ky+5.0
		if st.hovering && st.hoverX >= kx-6 && st.hoverX <= kx+keyW-18 &&
			st.hoverY >= rowTop && st.hoverY <= rowBottom {
			hovered = m
			cr.SetSourceRGBA(0.13, 0.40, 0.85, 0.13)
			cr.Rectangle(kx-6, rowTop, keyW-12, rowBottom-rowTop)
			cr.Fill()
		}

		cr.SetSourceRGB(c.r, c.g, c.b)
		cr.SetLineWidth(lw)
		cr.MoveTo(kx, ky-4)
		cr.LineTo(kx+24, ky-4)
		cr.Stroke()

		d := lastDepth(st.evals[m])
		name := st.sanOrUCI(m)
		if !isTop {
			// These fell out of the list at some point; say when they were
			// last seen, since their value belongs to that depth.
			name = fmt.Sprintf("%s (depth %d)", name, d)
		}
		text(kx+32, ky, size, isTop, c, name)

		label := fmt.Sprintf("%+.2f", st.evals[m][d])
		if n, ok := st.mates[m][d]; ok {
			label = fmt.Sprintf("#%+d", n)
		}
		textRight(kx+keyW-24, ky, size, isTop, c, label)

		ky += 19
		if i == topN-1 && topN < len(order) {
			ky += 10 // margin above the rule
			cr.SetSourceRGB(0.2, 0.2, 0.2)
			cr.SetLineWidth(1)
			cr.MoveTo(kx, ky-11)
			cr.LineTo(kx+keyW-24, ky-11)
			cr.Stroke()
			ky += 12 // margin below the rule
		}
		if ky > bottom {
			break
		}
	}

	// An arrow on the diagram for whichever move the pointer is resting on.
	if hovered != "" && boardSq > 0 {
		drawMoveArrow(cr, boardX, boardY, boardSq, hovered, st.flipped)
	}
}

// drawMoveArrow paints a thick translucent arrow from the move's origin square
// to its destination, in the board's current orientation.
func drawMoveArrow(cr *cairo.Context, x, y, sq float64, uci string, flipped bool) {
	if len(uci) < 4 {
		return
	}
	centre := func(file, rank int) (float64, float64) {
		col, row := file, 7-rank
		if flipped {
			col, row = 7-file, rank
		}
		return x + (float64(col)+0.5)*sq, y + (float64(row)+0.5)*sq
	}
	ff, fr := int(uci[0]-'a'), int(uci[1]-'1')
	tf, tr := int(uci[2]-'a'), int(uci[3]-'1')
	if ff < 0 || ff > 7 || fr < 0 || fr > 7 || tf < 0 || tf > 7 || tr < 0 || tr > 7 {
		return
	}
	x1, y1 := centre(ff, fr)
	x2, y2 := centre(tf, tr)
	dx, dy := x2-x1, y2-y1
	length := math.Hypot(dx, dy)
	if length < 1 {
		return
	}
	ux, uy := dx/length, dy/length

	head := sq * 0.62  // length of the arrowhead
	width := sq * 0.24 // thickness of the shaft
	shaftEnd := length - head
	if shaftEnd < 1 {
		shaftEnd = length * 0.4
	}

	cr.SetSourceRGBA(0.13, 0.40, 0.85, 0.55)
	cr.SetLineWidth(width)
	cr.SetLineJoin(cairo.LineJoinRound)
	cr.MoveTo(x1, y1)
	cr.LineTo(x1+ux*shaftEnd, y1+uy*shaftEnd)
	cr.Stroke()

	// arrowhead: a triangle centred on the destination square
	px, py := -uy, ux // perpendicular
	tipX, tipY := x2, y2
	baseX, baseY := x1+ux*shaftEnd, y1+uy*shaftEnd
	cr.MoveTo(tipX, tipY)
	cr.LineTo(baseX+px*sq*0.30, baseY+py*sq*0.30)
	cr.LineTo(baseX-px*sq*0.30, baseY-py*sq*0.30)
	cr.ClosePath()
	cr.Fill()
}

func (s *search) sanOrUCI(m string) string {
	if v, ok := s.san[m]; ok && v != "" {
		return v
	}
	return m
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// GUI
// ---------------------------------------------------------------------------

func main() {
	app := gtk.NewApplication("eu.imoma.rbevalgraph", gio.ApplicationNonUnique)
	app.ConnectActivate(func() { buildUI(app) })
	if code := app.Run(os.Args); code > 0 {
		os.Exit(code)
	}
}

func buildUI(app *gtk.Application) {
	st := newSearch()
	var run *runner

	win := gtk.NewApplicationWindow(app)
	win.SetTitle(winTitle)
	// Size is ours to choose; position is not. GTK4 removed window positioning
	// (gtk_window_move has no replacement, because Wayland forbids a client
	// from placing its own windows), so where this opens is up to the window
	// manager. On Xfce, Settings → Window Manager Tweaks → Placement centres
	// new windows if you want that.
	win.SetDefaultSize(startupSize())

	// A little left padding inside every value field, and the class used to
	// colour the FEN box when what it holds will not parse.
	css := gtk.NewCSSProvider()
	css.LoadFromData(`
entry, entry text, spinbutton, spinbutton text { padding-left: 8px; }
entry.path-entry, entry.path-entry text { padding-left: 3px; }
button.about-btn { padding-left: 14px; padding-right: 14px; }
entry.fen-bad, entry.fen-bad text { color: #cc0000; }
label.uci-options { font-family: monospace; font-size: 8.5pt; }
label.uci-pane-title { font-weight: bold; }
label.about-text { font-size: 11pt; }
label.probe-note { color: alpha(currentColor, 0.55); }
entry.probe-note, entry.probe-note text { color: #14357f; }
button.browsing label { font-family: monospace; letter-spacing: 1px; }
box.uci-pane {
  background-color: @theme_base_color;
  border: 1px solid alpha(currentColor, 0.35);
  border-radius: 8px;
}
`)
	if display := gdk.DisplayGetDefault(); display != nil {
		gtk.StyleContextAddProviderForDisplay(display, css,
			gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
	}

	root := gtk.NewBox(gtk.OrientationVertical, 8)
	root.SetMarginTop(8)
	root.SetMarginBottom(8)
	root.SetMarginStart(8)
	root.SetMarginEnd(8)

	// rows 1 and 2: the two text inputs. Each field is only as long as it
	// needs to be, Browse sits right beside the path it browses for, and the
	// action buttons are pushed out to the right edge. A size group on the two
	// labels makes both fields start at the same x.
	engineLabel := gtk.NewLabel("Engine:")
	fenLabel := gtk.NewLabel("FEN:")
	engineLabel.SetXAlign(0)
	fenLabel.SetXAlign(0)
	labelWidths := gtk.NewSizeGroup(gtk.SizeGroupHorizontal)
	labelWidths.AddWidget(engineLabel)
	labelWidths.AddWidget(fenLabel)

	row1 := gtk.NewBox(gtk.OrientationHorizontal, 6)
	engineEntry := gtk.NewEntry()
	engineEntry.SetWidthChars(72)
	engineEntry.AddCSSClass("path-entry")
	browse := gtk.NewButtonWithLabel("Browse…")
	infoBtn := iconButton(infoSVG, "i", "",
		"Show the UCI options this engine advertises.")
	infoBtn.SetSensitive(false)
	generate := gtk.NewButtonWithLabel("Generate eval graph")
	stopBtn := gtk.NewButtonWithLabel("Stop")
	stopBtn.SetSensitive(false)
	row1Spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	row1Spacer.SetHExpand(true)
	row1.Append(engineLabel)
	row1.Append(engineEntry)
	row1.Append(browse)
	row1.Append(infoBtn)
	row1.Append(row1Spacer)
	row1.Append(generate)
	row1.Append(stopBtn)

	row2 := gtk.NewBox(gtk.OrientationHorizontal, 6)
	fenEntry := gtk.NewEntry()
	fenEntry.SetWidthChars(72)
	fenEntry.AddCSSClass("path-entry")
	flipBtn := iconButton(toggleSVG, "⇅", "", "Toggle board view")
	aboutBtn := iconButton(aboutSVG, "i", "About", "About RBevalgraph")
	saveBtn := gtk.NewButtonWithLabel("Save PNG")
	saveBtn.SetSensitive(false)
	row2Spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	row2Spacer.SetHExpand(true)
	row2.Append(fenLabel)
	row2.Append(fenEntry)
	row2.Append(flipBtn)
	row2.Append(row2Spacer)
	row2.Append(aboutBtn)
	row2.Append(saveBtn)

	for _, b := range []*gtk.Button{browse, infoBtn, flipBtn, aboutBtn, generate,
		stopBtn, saveBtn} {
		b.SetMarginStart(4)
		b.SetMarginEnd(4)
	}
	browse.SetMarginStart(8)
	flipBtn.SetMarginStart(8) // same offset as Browse, so the two line up
	aboutBtn.AddCSSClass("about-btn")
	generate.SetMarginStart(28) // clear gap between Browse and the actions

	// Block a non-digit before it is ever inserted. The controller runs in the
	// capture phase, so it sees the key ahead of the entry and returning true
	// consumes it — nothing appears and nothing has to be removed again.
	// Keys that produce no character (arrows, Home, Tab, Backspace, Delete,
	// Return) and anything held with Ctrl or Alt are let through, so editing
	// and shortcuts still work.
	digitsOnlyKeys := func(sb *gtk.SpinButton) {
		keys := gtk.NewEventControllerKey()
		keys.SetPropagationPhase(gtk.PhaseCapture)
		keys.ConnectKeyPressed(func(keyval, keycode uint, state gdk.ModifierType) bool {
			switch {
			case state&(gdk.ControlMask|gdk.AltMask) != 0:
				return false // Ctrl-/Alt- shortcuts belong to the entry
			case keyval >= '0' && keyval <= '9':
				return false // the digits themselves
			case keyval >= gdk.KEY_KP_0 && keyval <= gdk.KEY_KP_9:
				return false // and the keypad's
			case keyval >= 0xff00 || keyval == gdk.KEY_ISO_Left_Tab:
				// Everything in the keysym function range, plus Shift+Tab,
				// which is ISO_Left_Tab just below it: Backspace, Delete,
				// Tab and Shift+Tab, Return, Escape, the arrows, Home, End,
				// Insert, Page Up/Down, and the keypad's editing keys. These
				// insert nothing, so they must all pass through — without them
				// the field cannot be corrected or left.
				return false
			}
			return true // a printable non-digit: consumed, never inserted
		})
		sb.AddController(keys)
	}

	// row 3: options and buttons
	row3 := gtk.NewBox(gtk.OrientationHorizontal, 0)

	// An empty label in the same size group as "Engine:" and "FEN:" takes
	// exactly their width, so the row starts where the input fields do
	// however wide those labels render in the current theme.
	row3Pad := gtk.NewLabel("")
	labelWidths.AddWidget(row3Pad)
	row3.Append(row3Pad)

	// Every spinner moves one unit per click — including page up/down, which
	// GTK would otherwise jump by ten — and speeds up while a button is held.
	// The climb rate is the acceleration, not a bigger step.
	byOne := func(sb *gtk.SpinButton, width int) *gtk.SpinButton {
		sb.SetIncrements(1, 1)
		sb.SetClimbRate(4)
		sb.SetWidthChars(width)
		// Typing is allowed, but only digits: GTK drops anything else, and an
		// out-of-range or unparseable entry reverts instead of being applied.
		sb.SetNumeric(true)
		sb.SetUpdatePolicy(gtk.UpdateIfValid)
		digitsOnlyKeys(sb)
		return sb
	}

	multipv := gtk.NewSpinButtonWithRange(1, 32, 1)
	byOne(multipv, 4)
	multipv.SetValue(6)
	secs := gtk.NewSpinButtonWithRange(1, 36000, 1)
	byOne(secs, 6)
	secs.SetValue(60)
	depth := gtk.NewSpinButtonWithRange(0, 99, 1)
	byOne(depth, 4)
	depth.SetValue(0)
	threads := gtk.NewSpinButtonWithRange(1, 256, 1)
	byOne(threads, 4)
	threads.SetValue(1)
	// A megabyte count, not free text: the spin button keeps it numeric and
	// inside a range the box can actually display.
	hashSpin := gtk.NewSpinButtonWithRange(1, hashCeiling, 1)
	byOne(hashSpin, 6)
	hashSpin.SetValue(256)
	// Each label+field pair lives in its own box, so the pair keeps air on
	// both sides and can be grayed-out as a unit when the engine has no such
	// option.
	addSet := func(label string, w gtk.Widgetter) *gtk.Box {
		box := gtk.NewBox(gtk.OrientationHorizontal, 6)
		box.SetMarginStart(10)
		box.SetMarginEnd(10)
		box.Append(gtk.NewLabel(label))
		box.Append(w)
		row3.Append(box)
		return box
	}
	engineEntry.SetTooltipText(
		"Path to a UCI engine binary. Its options are read as soon as the path " +
			"changes, and the controls below adapt to what that engine offers.")
	browse.SetTooltipText("Pick the engine binary from disk.")
	fenEntry.SetTooltipText(
		"The position to analyse, as a FEN. Turns red while it cannot be parsed.")
	multipv.SetTooltipText(
		"How many candidate moves the engine reports (UCI MultiPV). " +
			"Each one becomes a coloured line. " +
			"Grayed-out when the engine has no such option.")
	secs.SetTooltipText(
		"Wall-clock limit. The search is stopped after this many seconds, " +
			"whatever depth it has reached.")
	depth.SetTooltipText(
		"Stop once this depth is complete for every candidate. " +
			"0 switches the depth limit off and lets the seconds decide.")
	hashSpin.SetTooltipText(
		"Transposition table size in megabytes (UCI Hash). " +
			"Grayed-out when the engine has no such option.")
	threads.SetTooltipText(
		"Search threads (UCI Threads). " +
			"Grayed-out when the engine has no such option.")
	generate.SetTooltipText("Start the search and plot it as it goes.")
	stopBtn.SetTooltipText(
		"Stop the engine now. The graph keeps everything found so far.")
	saveBtn.SetTooltipText("Write the graph as it stands to a PNG file.")

	multipvBox := addSet("MultiPV:", multipv)
	// The other sets keep their 10px separation; this one lines up with the
	// fields above, which sit 6px after their label.
	multipvBox.SetMarginStart(6)
	addSet("Max seconds:", secs)
	addSet("Max depth:", depth)
	hashBox := addSet("Hash:", hashSpin)
	// The typed value is a plain megabyte count on every engine I know of, so
	// it gets a unit. A combo lists its own labels (auto-size, 256Mb, …) and
	// the unit is hidden for it.
	hashUnit := gtk.NewLabel("Mb")
	hashBox.Append(hashUnit)
	hashUnit.SetTooltipText("Megabytes.")
	// Shown in place of the value when the engine has no Hash option at all.
	hashNone := gtk.NewLabel("---")
	hashBox.Append(hashNone)
	hashNone.SetVisible(false)
	threadsBox := addSet("Threads:", threads)

	// Hash is a plain value on most engines and a fixed set on some (RBp4wn
	// offers auto-size / 4Mb / … / 256Mb). The dropdown replaces the entry
	// whenever the engine declares Hash as a combo.
	var hashDrop *gtk.DropDown
	var hashVars []string
	hashValue := func() string {
		if hashDrop != nil && len(hashVars) > 0 {
			i := int(hashDrop.Selected())
			if i >= 0 && i < len(hashVars) {
				return hashVars[i]
			}
		}
		return strconv.Itoa(int(hashSpin.Value()))
	}

	spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	spacer.SetHExpand(true)
	row3.Append(spacer)

	area := gtk.NewDrawingArea()
	area.SetHExpand(true)
	area.SetVExpand(true)
	area.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, w, h int) {
		draw(cr, w, h, st)
	})

	// Hovering a move in the list highlights it and draws its arrow on the
	// diagram. The canvas reports the pointer; the drawing code decides what
	// is under it, using the row strips it is laying out anyway.
	motion := gtk.NewEventControllerMotion()
	motion.ConnectMotion(func(x, y float64) {
		st.mu.Lock()
		moved := !st.hovering || math.Abs(st.hoverX-x) > 0.5 || math.Abs(st.hoverY-y) > 0.5
		st.hoverX, st.hoverY, st.hovering = x, y, true
		st.mu.Unlock()
		if moved {
			area.QueueDraw()
		}
	})
	motion.ConnectLeave(func() {
		st.mu.Lock()
		was := st.hovering
		st.hovering = false
		st.mu.Unlock()
		if was {
			area.QueueDraw()
		}
	})
	area.AddController(motion)

	root.Append(row1)
	root.Append(row2)
	root.Append(row3)
	root.Append(area)
	overlay := gtk.NewOverlay()
	overlay.SetChild(root)
	win.SetChild(overlay)

	redraw := func() { area.QueueDraw() }

	// Where the browsers last were: one folder for engines, one for graphs.
	lastEngineDir := ""
	lastPNGDir := ""
	// A combo Hash remembers which of the engine's choices was picked, and a
	// spin Hash its number — both only for the engine they were saved with.
	savedHashChoice := ""
	savedHashMB := 0
	savedHashFor := ""

	// Probe bookkeeping: the path last probed, the one that answered, and the
	// one that failed (so its text can be shown in red).
	probedPath := ""
	probedOK := ""
	probedBad := ""

	// Whether the two required inputs are currently usable. Generate stays
	// grayed-out until both are.
	engineOK, fenOK := false, false

	// True from the moment an engine is asked what it is until the answer, or
	// the timeout, comes back.
	uiProbing := false

	// Re-applies the window's enabled/disabled state. Assigned once setBusy
	// exists further down; the mask helpers above it call it through here.
	var refreshLock func()

	// The engine's advertised options, shown once after it is loaded and again
	// whenever the info button is pressed.
	optionsText := ""
	optionsTitle := ""

	// While an engine is being identified the field shows what the program is
	// doing instead of the path — identifying can take a while, since the UCI
	// attempt waits ten seconds before the XBoard one begins. The real path is
	// held here meanwhile, and enginePath() is what every other piece of code
	// reads, so nothing ever sees the message in place of the path.
	maskedPath := ""
	entryMasked := false

	// While detecting, the Browse button's label runs through these frames so
	// there is movement somewhere on screen — a detection can take a quarter
	// of a minute and otherwise nothing would look alive.
	browseFrames := []string{"||||", " |||", "  ||", "   |"}
	browseAnim := false
	startBrowseAnim := func() {
		if browseAnim {
			return
		}
		browseAnim = true
		// Freeze the button at its present width so the row does not twitch
		// as the frames change.
		if w := browse.Width(); w > 0 {
			browse.SetSizeRequest(w, -1)
		}
		browse.AddCSSClass("browsing")
		frame := 0
		glib.TimeoutAdd(300, func() bool {
			if !browseAnim {
				return false // stop the timer
			}
			browse.SetLabel(browseFrames[frame%len(browseFrames)])
			frame++
			return true
		})
	}
	stopBrowseAnim := func() {
		if !browseAnim {
			return
		}
		browseAnim = false
		browse.SetLabel("Browse…")
		browse.RemoveCSSClass("browsing")
		browse.SetSizeRequest(-1, -1)
	}

	enginePath := func() string {
		if entryMasked {
			return maskedPath
		}
		return strings.TrimSpace(engineEntry.Text())
	}
	maskEngineEntry := func(msg string) {
		uiProbing = true
		startBrowseAnim()
		if refreshLock != nil {
			refreshLock()
		}
		if !entryMasked {
			maskedPath = strings.TrimSpace(engineEntry.Text())
			entryMasked = true
			engineEntry.SetEditable(false)
			engineEntry.AddCSSClass("probe-note")
		}
		engineEntry.SetText(msg)
	}
	unmaskEngineEntry := func() {
		uiProbing = false
		stopBrowseAnim()
		if refreshLock != nil {
			refreshLock()
		}
		if !entryMasked {
			return
		}
		entryMasked = false
		engineEntry.SetText(maskedPath)
		engineEntry.SetEditable(true)
		engineEntry.RemoveCSSClass("probe-note")
	}

	// What the currently chosen engine actually offers. Everything is assumed
	// available until an engine has been probed.
	hasMultiPV, hasThreads, hasHash := true, true, true

	// One place decides what is clickable, so the window can never end up in a
	// half-enabled state: everything that configures a search is live only
	// while no search runs, Stop only while one does, and Save PNG only once
	// there is something to save.
	setBusy := func(busy bool) {
		// Two reasons to lock the window: a search is running, or an engine is
		// being identified. During detection the engine field stays live —
		// it is carrying the message — and so does About; everything else is
		// grayed-out, including Browse, whose label is doing the animating.
		idle := !busy && !uiProbing

		for _, w := range []interface{ SetSensitive(bool) }{
			browse, fenEntry, multipv, secs, depth, hashSpin, threads,
		} {
			w.SetSensitive(idle)
		}
		engineEntry.SetSensitive(!busy)
		flipBtn.SetSensitive(idle)
		aboutBtn.SetSensitive(true) // always reachable

		// Nothing to generate from until there is an engine and a position.
		generate.SetSensitive(idle && engineOK && fenOK)
		// An option set is live only when this engine has it and nothing is
		// running, so the two reasons for greying never fight each other.
		multipvBox.SetSensitive(idle && hasMultiPV)
		hashBox.SetSensitive(idle && hasHash)
		threadsBox.SetSensitive(idle && hasThreads)
		stopBtn.SetSensitive(busy)
		infoBtn.SetSensitive(idle && optionsText != "")
		st.mu.Lock()
		haveData := len(st.seen) > 0
		st.mu.Unlock()
		// Clickable once a search has finished — by Stop, by the time cap or
		// by the engine itself — and actually produced something.
		saveBtn.SetSensitive(idle && haveData)
	}

	refreshLock = func() {
		st.mu.Lock()
		running := st.running
		st.mu.Unlock()
		setBusy(running)
	}

	fail := func(msg string) {
		st.mu.Lock()
		st.status = msg
		st.mu.Unlock()
		setBusy(false)
		redraw()
	}

	// Put every control into its starting state. Without this the window opens
	// with whatever GTK defaults to, which left Generate clickable before an
	// engine and a FEN had been given.
	setBusy(false)

	// The pane opens on every successful probe, including the one for the
	// engine restored from config.yaml at startup — an engine that has loaded
	// shows what it can do, whichever way it got there.
	// The pane is laid over the app window rather than being a window of its
	// own. GTK4 has no window-positioning call — gtk_window_move was removed
	// because Wayland forbids it — so a separate window is placed by the
	// window manager and cannot be centred from here. As an overlay child it
	// is centred on the app window by construction, whatever the window
	// manager does, and it moves and resizes with it.
	paneTitle := gtk.NewLabel("")
	paneTitle.SetXAlign(0)
	paneTitle.SetHExpand(true)
	paneTitle.AddCSSClass("uci-pane-title")
	paneClose := gtk.NewButtonWithLabel("✕")
	paneClose.SetTooltipText("Close")
	// Sits top left of the About pane and swaps it for the longer explanation.
	paneInfo := gtk.NewButtonWithLabel("INFO")
	paneInfo.SetTooltipText("What this program does")
	paneInfo.SetVisible(false)
	paneInfo.SetMarginEnd(8)

	// Height taken by the pane's own header row, allowed for when the pane is
	// sized to its content.
	const paneHeadHeight = 52

	paneHead := gtk.NewBox(gtk.OrientationHorizontal, 6)
	paneHead.SetMarginTop(10)
	paneHead.SetMarginBottom(4)
	paneHead.SetMarginStart(22)
	paneHead.SetMarginEnd(10)
	paneHead.Append(paneInfo)
	paneHead.Append(paneTitle)
	paneHead.Append(paneClose)

	paneBody := gtk.NewLabel("")
	paneBody.SetXAlign(0)
	paneBody.SetHAlign(gtk.AlignStart)
	paneBody.SetVAlign(gtk.AlignStart) // text starts at the top, never centred
	paneBody.SetSelectable(true)
	// A selectable label takes focus when it appears and selects all of
	// itself; leaving it unfocusable avoids that while still allowing
	// selection with the mouse.
	paneBody.SetCanFocus(false)
	paneBody.AddCSSClass("uci-options")
	paneBody.SetMarginTop(8)
	paneBody.SetMarginStart(22)
	paneBody.SetMarginEnd(22)
	paneBody.SetMarginBottom(18)

	paneScroll := gtk.NewScrolledWindow()
	paneScroll.SetHExpand(true)
	paneScroll.SetVExpand(true)
	// Report the text's own natural size as the scroller's, so the pane grows
	// to fit its content. GTK measures during the layout pass, when the fonts
	// and CSS for the content are settled — measuring it ourselves at the
	// moment of showing gave the wrong answer whenever the style had just
	// changed, which is why the About text sometimes arrived with a
	// scrollbar it did not need.
	paneScroll.SetPropagateNaturalWidth(true)
	paneScroll.SetPropagateNaturalHeight(true)
	paneScroll.SetChild(paneBody)

	pane := gtk.NewBox(gtk.OrientationVertical, 0)
	pane.AddCSSClass("uci-pane")
	pane.SetHAlign(gtk.AlignCenter)
	pane.SetVAlign(gtk.AlignCenter)
	pane.SetVisible(false)
	pane.Append(paneHead)
	pane.Append(paneScroll)
	overlay.AddOverlay(pane)

	closeOptions := func() {
		pane.SetVisible(false)
		root.SetSensitive(true) // the app is usable again
	}
	paneClose.ConnectClicked(func() { closeOptions() })

	// Escape closes it too.
	paneKeys := gtk.NewEventControllerKey()
	paneKeys.SetPropagationPhase(gtk.PhaseCapture)
	paneKeys.ConnectKeyPressed(func(keyval, keycode uint, state gdk.ModifierType) bool {
		if pane.Visible() && keyval == gdk.KEY_Escape {
			closeOptions()
			return true
		}
		return false
	})
	win.AddController(paneKeys)

	// showPane puts either kind of content in the overlay: the engine's option
	// list, monospace and top-left, or the About text, proportional and
	// centred both ways.
	// showPane puts either kind of content in the overlay. fit makes the pane
	// shrink to its content instead of taking a fixed share of the window.
	// showPane fills the overlay and sizes it to whatever it is showing, with
	// an even margin around the text. The window only ever acts as a ceiling:
	// content larger than that scrolls.
	showPane := func(title, body string, mode int) {
		paneTitle.SetText(title)
		switch mode {
		case paneProse:
			// A block of explanation: wrapped, left-aligned, from the top.
			paneBody.RemoveCSSClass("uci-options")
			paneBody.AddCSSClass("about-text")
			paneBody.SetHAlign(gtk.AlignStart)
			paneBody.SetVAlign(gtk.AlignStart)
			paneBody.SetXAlign(0)
			paneBody.SetJustify(gtk.JustifyLeft)
			paneBody.SetWrap(true)
			paneBody.SetMaxWidthChars(76)
			paneBody.SetMarkup(body)
		case paneCentred:
			paneBody.RemoveCSSClass("uci-options")
			paneBody.AddCSSClass("about-text")
			paneBody.SetHAlign(gtk.AlignCenter)
			paneBody.SetVAlign(gtk.AlignCenter)
			paneBody.SetXAlign(0.5)
			paneBody.SetJustify(gtk.JustifyCenter)
			paneBody.SetWrap(true)
			paneBody.SetMaxWidthChars(52) // so the long line breaks sensibly
			paneBody.SetMarkup(body)
		default:
			paneBody.RemoveCSSClass("about-text")
			paneBody.AddCSSClass("uci-options")
			paneBody.SetHAlign(gtk.AlignStart)
			paneBody.SetVAlign(gtk.AlignStart)
			paneBody.SetXAlign(0)
			paneBody.SetJustify(gtk.JustifyLeft)
			paneBody.SetWrap(false)
			paneBody.SetMaxWidthChars(-1)
			paneBody.SetText(body)
		}
		// The window is the ceiling: past it the scrollbars take over.
		w, h := win.Width(), win.Height()
		if w < 200 || h < 200 { // not mapped yet
			w, h = 1280, 800
		}
		paneScroll.SetMaxContentWidth(w - 80)
		paneScroll.SetMaxContentHeight(h - 80 - paneHeadHeight)
		paneScroll.SetMinContentWidth(320)
		paneScroll.SetMinContentHeight(120)
		pane.SetSizeRequest(-1, -1) // GTK works the size out from the content

		pane.SetVisible(true)
		// Nothing behind the pane can be reached until it is closed.
		root.SetSensitive(false)
	}

	showOptions := func() {
		if optionsText == "" {
			return
		}
		showPane(optionsTitle, optionsText, paneMono)
		paneInfo.SetVisible(false)
	}

	showAbout := func() {
		showPane("", aboutText, paneCentred)
		paneInfo.SetLabel("INFO")
		paneInfo.SetVisible(true)
	}

	showInfo := func() {
		showPane("", infoText, paneProse)
		paneInfo.SetVisible(false) // nothing to go back to; close the pane
	}

	infoBtn.ConnectClicked(showOptions)
	aboutBtn.ConnectClicked(showAbout)
	paneInfo.ConnectClicked(func() { showInfo() })

	// Reshape the option row to what this engine actually advertises: a set
	// that the engine does not have is grayed-out instead of silently ignored,
	// and a combo-typed Hash becomes a dropdown of the engine's own choices.
	applyOptions := func(opts map[string]uciOption) {
		var hash uciOption
		_, hasMultiPV = opts["MultiPV"]
		if !hasMultiPV {
			multipv.SetValue(1) // the engine will only ever give one line
		}
		_, hasThreads = opts["Threads"]
		if !hasThreads {
			threads.SetValue(1)
		}
		hash, hasHash = opts["Hash"]

		wantCombo := hasHash && hash.typ == "combo" && len(hash.vars) > 0
		if hashDrop != nil {
			hashBox.Remove(hashDrop)
			hashDrop = nil
			hashVars = nil
		}
		hashNone.SetVisible(!hasHash)
		hashUnit.SetVisible(hasHash && !wantCombo)
		if !hasHash {
			hashSpin.SetVisible(false)
		}
		if wantCombo {
			hashSpin.SetVisible(false)
			hashVars = hash.vars
			hashDrop = gtk.NewDropDownFromStrings(hashVars)
			hashDrop.SetSizeRequest(130, -1)
			hashDrop.SetTooltipText(
				"Transposition table size (UCI Hash). This engine offers a " +
					"fixed set of values.")
			want := hash.def
			if savedHashChoice != "" {
				want = savedHashChoice
			}
			for i, v := range hashVars {
				if v == want {
					hashDrop.SetSelected(uint(i))
					break
				}
			}
			hashBox.Append(hashDrop)
		} else if hasHash {
			hashSpin.SetVisible(true)
			// Follow the engine's own bounds where it gives them, but never
			// wider than the field can show.
			lo, hi := 1.0, hashCeiling
			if hash.hasRange {
				if hash.min > 0 {
					lo = hash.min
				}
				if hash.max > 0 && hash.max < hi {
					hi = hash.max
				}
			}
			if hi < lo {
				hi = lo
			}
			hashSpin.SetRange(lo, hi)
			hashSpin.SetIncrements(1, 1) // SetRange keeps them, but be explicit
			v, err := strconv.ParseFloat(hash.def, 64)
			if savedHashMB > 0 && probedPath == savedHashFor {
				v, err = float64(savedHashMB), nil
			}
			if err == nil && hasHash {
				if v < lo {
					v = lo
				}
				if v > hi {
					v = hi
				}
				hashSpin.SetValue(v)
			}
		}
		st.mu.Lock()
		busy := st.running
		st.mu.Unlock()
		setBusy(busy)
	}

	// Probe an engine as soon as its path changes to something runnable, so the
	// controls describe that engine before anything is searched.
	probeIfNeeded := func() {
		path := enginePath()
		if path == probedPath {
			return
		}
		st.mu.Lock()
		busy := st.running
		st.mu.Unlock()
		if busy {
			return
		}
		probedPath = path
		probedOK = ""
		probedBad = ""
		if path == "" {
			applyOptions(nil)
			unmaskEngineEntry()
			st.mu.Lock()
			st.probing = false
			st.engineID = ""
			st.status = "idle — browse to a UCI engine and enter a FEN"
			st.mu.Unlock()
			redraw()
			return
		}
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			probedBad = path // no such file, a directory, or not executable
			applyOptions(nil)
			unmaskEngineEntry()
			st.mu.Lock()
			st.probing = false
			st.engineID = ""
			st.mu.Unlock()
			setBusy(false)
			redraw()
			return
		}
		st.mu.Lock()
		st.probing = true
		st.status = "..detecting engine protocol — trying UCI, up to 10 sec of silence.."
		st.mu.Unlock()
		maskEngineEntry("[ detecting engine protocol — trying UCI, up to 10 sec ]")
		redraw()

		go func(p string) {
			res, err := probeEngine(p)
			// A binary that will not speak UCI may still be an XBoard engine;
			// worth saying which, rather than only that it is not UCI. Some
			// engines take their time, so say which protocol is being tried.
			var xb xboardResult
			if err != nil {
				glib.IdleAdd(func() {
					st.mu.Lock()
					st.status = "..no UCI answer — trying XBoard protocol, up to 10 sec of silence.."
					st.mu.Unlock()
					maskEngineEntry("[ no UCI answer — trying XBoard, up to 10 sec ]")
					redraw()
				})
				xb = probeXBoard(p, func(what string) {
					if what != "initialising" {
						return
					}
					// The engine asked for more time; say so rather than
					// leaving the user watching a stale message.
					glib.IdleAdd(func() {
						st.mu.Lock()
						st.status = "..XBoard engine is initialising, it asked " +
							"for more time — waiting up to 60 sec.."
						st.mu.Unlock()
						maskEngineEntry("[ XBoard engine is initialising — " +
							"waiting up to 60 sec ]")
						redraw()
					})
				})
			}
			glib.IdleAdd(func() {
				showPopup := false
				if enginePath() != p {
					// The user moved on while we were asking. A newer probe
					// owns the flag now unless the field was emptied, so only
					// clear it if nothing else is running.
					st.mu.Lock()
					if enginePath() == "" {
						st.probing = false
						unmaskEngineEntry()
					}
					st.mu.Unlock()
					redraw()
					return
				}
				unmaskEngineEntry()
				applyOptions(res.options)
				st.mu.Lock()
				st.probing = false
				if err != nil {
					// Not a UCI engine, or not one that will talk to us. Say
					// so plainly and leave Generate unavailable.
					probedBad = p
					optionsText = ""
					optionsTitle = ""
					st.engineID = ""
					if xb.isXBoard {
						name := xb.name
						if name == "" {
							name = filepath.Base(p)
						}
						optionsTitle = name + "  —  XBoard engine"
						optionsText = fmt.Sprintf("%s answers the XBoard "+
							"(CECP) protocol, not UCI. "+
							"It initialised in %.1f seconds.\n\n",
							name, xb.took.Seconds()) +
							"RBevalgraph can only handle an UCI engine.\n\n" +
							"Its reply to \"xboard\" and \"protover 2\":\n\n" +
							strings.Join(xb.raw, "\n")
						// Always shown, even for the engine restored from
						// config.yaml at startup: this one is a warning that
						// the engine cannot be used, not a listing the user
						// may or may not care to see.
						showPopup = true
						st.status = fmt.Sprintf("%s is an XBoard engine, "+
							"initialised in %.1f seconds — RBevalgraph can "+
							"only handle an UCI engine", name, xb.took.Seconds())
					} else {
						st.status = filepath.Base(p) +
							" does not answer as a UCI engine — " + err.Error()
					}
				} else {
					probedOK = p
					if res.name != "" {
						st.engineID = res.name
					} else {
						st.engineID = filepath.Base(p)
					}
					st.status = fmt.Sprintf("ready: %s (initialised in %.1f seconds)",
						st.engineID, res.took.Seconds())
					optionsText = strings.Join(res.raw, "\n")
					optionsTitle = st.engineID
					showPopup = optionsText != ""
				}
				st.mu.Unlock()
				infoBtn.SetSensitive(optionsText != "")
				setBusy(false)
				redraw()
				if showPopup {
					showOptions()
				}
			})
		}(path)
	}

	// Twice a second, tint the FEN box red while its contents cannot be parsed.
	// An empty box is not an error yet, so it stays in the normal colour.
	// Settings from the last session. Until an engine has answered we do not
	// know what it supports, so the option row starts in its unknown state:
	// Hash reads ---, and the three engine-dependent sets are grayed-out.
	cfg := loadConfig()
	applyOptions(nil)
	engineEntry.SetText(cfg.Engine)
	fenEntry.SetText(cfg.FEN)
	if cfg.MultiPV > 0 {
		multipv.SetValue(float64(cfg.MultiPV))
	}
	if cfg.MaxSeconds > 0 {
		secs.SetValue(float64(cfg.MaxSeconds))
	}
	depth.SetValue(float64(cfg.MaxDepth))
	if cfg.HashMB > 0 {
		hashSpin.SetValue(float64(cfg.HashMB))
	}
	// The remembered Hash is restored only for the engine it was saved with;
	// a different engine brings its own default.
	savedHashMB, savedHashFor = cfg.HashMB, cfg.Engine
	if cfg.Threads > 0 {
		threads.SetValue(float64(cfg.Threads))
	}
	lastEngineDir, lastPNGDir = cfg.LastEngineDir, cfg.LastPNGDir
	savedHashChoice = cfg.HashChoice

	// Whatever is on screen is written back whenever it changes, so the next
	// start comes up the same way.
	saved := cfg
	persist := func() {
		now := config{
			Engine:        enginePath(),
			FEN:           strings.TrimSpace(fenEntry.Text()),
			MultiPV:       int(multipv.Value()),
			MaxSeconds:    int(secs.Value()),
			MaxDepth:      int(depth.Value()),
			HashMB:        int(hashSpin.Value()),
			HashChoice:    hashValue(),
			Threads:       int(threads.Value()),
			LastEngineDir: lastEngineDir,
			LastPNGDir:    lastPNGDir,
		}
		if now == saved {
			return
		}
		saved = now
		if err := now.save(); err != nil {
			st.mu.Lock()
			st.status = "could not write config.yaml: " + err.Error()
			st.mu.Unlock()
			redraw()
		}
	}

	// What the plot on screen was built from, so a change can invalidate it.
	lastEngineText, lastFenText := "", ""

	fenBad, engineBad := false, false
	glib.TimeoutAdd(500, func() bool {
		probeIfNeeded()

		text := strings.TrimSpace(fenEntry.Text())
		bad := text != "" && checkFEN(text) != ""
		if bad != fenBad {
			fenBad = bad
			if bad {
				fenEntry.AddCSSClass("fen-bad")
			} else {
				fenEntry.RemoveCSSClass("fen-bad")
			}
		}

		// A different engine or a different position means the plot on screen
		// no longer belongs to what the controls say. Clear it, drop back to
		// the intro text, and grey Save PNG out again.
		engText := enginePath()
		if engText != lastEngineText || text != lastFenText {
			lastEngineText, lastFenText = engText, text
			st.mu.Lock()
			busy := st.running
			if !busy {
				hadData := len(st.seen) > 0
				st.reset()
				st.meta = ""
				if text != "" && checkFEN(text) == "" {
					// Valid position: show its diagram straight away, seen
					// from the side that is to move.
					st.fen = text
					st.board, st.sideToMove = parseFENBoard(text)
					st.flipped = st.sideToMove == 'b'
				} else {
					st.fen = ""
					st.board = [8][8]rune{}
				}
				// Do not stamp on the protocol-detection message; that one is
				// telling the user why the program is busy.
				if hadData && !st.probing {
					st.status = "engine or position changed — plot cleared"
				}
			}
			st.mu.Unlock()
			if !busy {
				setBusy(false)
				redraw()
			}
		}

		// An engine path that is not a runnable UCI engine is shown in red,
		// the same way an unparseable FEN is.
		engBad := engText != "" && engText == probedBad
		if engBad != engineBad {
			engineBad = engBad
			if engBad {
				engineEntry.AddCSSClass("fen-bad")
			} else {
				engineEntry.RemoveCSSClass("fen-bad")
			}
		}

		// Re-check what Generate needs, and update it when that changed.
		wasEngine, wasFen := engineOK, fenOK
		fenOK = text != "" && checkFEN(text) == ""
		engineOK = false
		if p := enginePath(); p != "" && p == probedOK {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				engineOK = true
			}
		}
		if engineOK != wasEngine || fenOK != wasFen {
			st.mu.Lock()
			busy := st.running
			st.mu.Unlock()
			setBusy(busy)
		}
		persist()
		return true
	})

	browse.ConnectClicked(func() {
		fc := gtk.NewFileChooserNative("Select UCI engine", &win.Window,
			gtk.FileChooserActionOpen, "Select", "Cancel")

		// Start where an engine was last picked; failing that, beside the one
		// currently in the field.
		startDir := lastEngineDir
		if startDir == "" {
			if p := enginePath(); p != "" {
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					startDir = filepath.Dir(p)
				}
			}
		}
		if startDir != "" {
			// A folder that has since gone is not worth a complaint; the
			// dialog just opens wherever GTK would have put it.
			_ = fc.SetCurrentFolder(gio.NewFileForPath(startDir))
		}

		fc.ConnectResponse(func(id int) {
			if gtk.ResponseType(id) == gtk.ResponseAccept {
				if f := fc.File(); f != nil {
					// A probe may still be running with the field masked;
					// restore it first so the new path is not swallowed.
					unmaskEngineEntry()
					engineEntry.SetText(f.Path())
					lastEngineDir = filepath.Dir(f.Path())
				}
			}
			fc.Destroy()
		})
		fc.Show()
	})

	generate.ConnectClicked(func() {
		path := enginePath()
		if path == "" {
			fail("no engine chosen — type a path or press Browse")
			return
		}
		fi, err := os.Stat(path)
		switch {
		case err != nil:
			fail("cannot read that path: " + err.Error())
			return
		case fi.IsDir():
			fail("that is a directory, not an engine binary: " + path)
			return
		case fi.Mode()&0o111 == 0:
			fail("that file is not executable: " + path)
			return
		}

		fen := strings.TrimSpace(fenEntry.Text())
		if fen == "" {
			fail("no FEN given — paste the position you want analysed")
			return
		}
		if msg := checkFEN(fen); msg != "" {
			fail(msg)
			return
		}

		cfg := runConfig{
			enginePath: path,
			fen:        fen,
			multipv:    int(multipv.Value()),
			maxSeconds: secs.Value(),
			maxDepth:   int(depth.Value()),
			hash:       hashValue(),
			threads:    int(threads.Value()),
		}

		// Where the .log goes: beside the graphs if a folder has been used for
		// those, otherwise beside the program, which is also where
		// config.yaml lives.
		cfg.onDone = func(transcript []string) {
			st.mu.Lock()
			name := strings.TrimSuffix(st.suggestedPNGName(), ".png") + ".log"
			header := []string{
				"RBevalgraph search log",
				time.Now().Format("2006-01-02 15:04:05"),
				"engine: " + st.engineID + "  (" + cfg.enginePath + ")",
				"FEN:    " + cfg.fen,
				fmt.Sprintf("MultiPV %d  ·  Hash %s  ·  Threads %d  ·  cap %.0fs%s",
					cfg.multipv, cfg.hash, cfg.threads, cfg.maxSeconds,
					map[bool]string{true: fmt.Sprintf(" / depth %d", cfg.maxDepth)}[cfg.maxDepth > 0]),
			}
			st.mu.Unlock()

			dir := lastPNGDir
			if dir == "" {
				dir = filepath.Dir(configPath())
			}
			full := filepath.Join(dir, name)

			var note string
			if err := writeSearchLog(full, header, transcript); err != nil {
				note = "  ·  could not write the log: " + err.Error()
			} else {
				note = "  ·  log: " + full
			}
			st.mu.Lock()
			st.status += note
			st.mu.Unlock()
			glib.IdleAdd(redraw)
		}

		st.mu.Lock()
		st.reset()
		st.fen = cfg.fen
		st.board, st.sideToMove = parseFENBoard(cfg.fen)
		st.multipv = cfg.multipv
		st.maxDepth = cfg.maxDepth
		st.maxSecs = cfg.maxSeconds
		st.meta = fmt.Sprintf("MultiPV %d  ·  Hash %s  ·  Threads %d  ·  cap %.0fs",
			cfg.multipv, cfg.hash, cfg.threads, cfg.maxSeconds)
		if cfg.maxDepth > 0 {
			st.meta += fmt.Sprintf(" / depth %d", cfg.maxDepth)
		}
		st.status = "starting engine…"
		// Marked busy before the goroutine exists, so a handshake that fails in
		// the first milliseconds cannot leave the controls disabled.
		st.running = true
		st.mu.Unlock()
		setBusy(true)
		redraw()

		r, err := start(cfg, st, redraw)
		if err != nil {
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
			fail("could not start the engine: " + err.Error())
			return
		}
		run = r

		// Hand the controls back as soon as the search goroutine is done,
		// however it ended.
		glib.TimeoutAdd(200, func() bool {
			st.mu.Lock()
			running := st.running
			st.mu.Unlock()
			setBusy(running)
			redraw()
			return running
		})
	})

	flipBtn.ConnectClicked(func() {
		st.mu.Lock()
		st.flipped = !st.flipped
		st.mu.Unlock()
		redraw()
	})

	stopBtn.ConnectClicked(func() {
		if run == nil {
			return
		}
		run.halt()
	})

	saveBtn.ConnectClicked(func() {
		fc := gtk.NewFileChooserNative("Save graph as PNG", &win.Window,
			gtk.FileChooserActionSave, "Save", "Cancel")
		st.mu.Lock()
		suggested := st.suggestedPNGName()
		st.mu.Unlock()
		fc.SetCurrentName(suggested)

		// Start in the folder of the previous save; before there is one, the
		// folder the engine was browsed from is the next best guess.
		startDir := lastPNGDir
		if startDir != "" {
			if err := fc.SetCurrentFolder(gio.NewFileForPath(startDir)); err != nil {
				// Not worth bothering the user about; the dialog just opens
				// wherever GTK would have put it.
				startDir = ""
			}
		}
		fc.ConnectResponse(func(id int) {
			if gtk.ResponseType(id) == gtk.ResponseAccept {
				if f := fc.File(); f != nil {
					// Draw at exactly the size the canvas has on screen, so
					// the file is the picture in front of you — the same plot
					// width, the same number of rows in the list. The surface
					// is pngScale times larger and the context scaled to
					// match, which keeps the text crisp without changing the
					// layout by a single pixel.
					w, h := area.Width(), area.Height()
					if w < 200 || h < 200 { // not mapped yet
						w, h = 1280, 700
					}
					surface := cairo.CreateImageSurface(cairo.FormatARGB32,
						w*pngScale, h*pngScale)
					cr := cairo.Create(surface)
					cr.Scale(pngScale, pngScale)
					draw(cr, w, h, st)
					surface.Flush()
					if err := surface.WriteToPNG(f.Path()); err != nil {
						st.mu.Lock()
						st.status = "could not write the PNG: " + err.Error()
						st.mu.Unlock()
					} else {
						lastPNGDir = filepath.Dir(f.Path())
						st.mu.Lock()
						st.status = "wrote " + f.Path()
						st.mu.Unlock()
					}
					redraw()
				}
			}
			fc.Destroy()
		})
		fc.Show()
	})

	win.SetVisible(true)
}
