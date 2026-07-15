package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"git.sr.ht/~rockorager/vaxis"
	vxspinner "git.sr.ht/~rockorager/vaxis/widgets/spinner"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func main() {
	var slogWriter io.Writer = &bytes.Buffer{}
	if logPath := os.Getenv("CMENU_LOG_PATH"); logPath != "" {
		logFile, err := os.Create(logPath)
		if err != nil {
			panic(err)
		}
		slogWriter = logFile
	}
	{
		slogHandler := slog.NewJSONHandler(slogWriter, nil)
		slogLogger := slog.New(slogHandler)
		slog.SetDefault(slogLogger)
	}

	var quitErr error
	defer func() {
		if quitErr != nil {
			slog.Error("quit due to error", "error", quitErr.Error())
		}
		if buf, ok := slogWriter.(*bytes.Buffer); ok {
			io.Copy(os.Stderr, buf)
		}
		if quitErr != nil {
			os.Exit(1)
		}
	}()

	if len(os.Args) > 1 {
		switch cmd := os.Args[1]; cmd {
		case markerHighlight, markerStay, markerLabel:
			fmt.Print(oscPrefix + cmd + oscTerm)
			return
		case "image":
			if len(os.Args) != 3 {
				quitErr = fmt.Errorf("image needs argument")
				return
			}
			switch file := os.Args[2]; file {
			case "-":
				fmt.Print(oscPrefix + markerImageData + ";")
				enc := base64.NewEncoder(base64.StdEncoding, os.Stdout)
				io.Copy(enc, os.Stdin)
				enc.Close()
				fmt.Print(oscTerm)
				return
			default:
				fmt.Print(oscPrefix + markerImagePath + ";" + file + oscTerm)
				return
			}
		default:
			quitErr = fmt.Errorf("unknown command %q", cmd)
			return
		}
	}

	configDir, _ := os.UserConfigDir()
	confPath := filepath.Join(configDir, "cmenu", "config.toml")

	conf, err := parseConfig(confPath)
	if err != nil {
		quitErr = err
		return
	}

	var scripts = map[string]*script{}
	var scriptOrder = make([]string, 0, len(conf.Scripts))
	for _, sconf := range conf.Scripts {
		scripts[sconf.Name] = &script{scriptConf: sconf}
		scriptOrder = append(scriptOrder, sconf.Name)
	}

	var (
		triggersOnStart  = map[ /* script name */ string]struct{}{}
		triggersPrefix   = map[ /* prefix */ string] /* script names */ []string{}
		triggersScript   = map[ /* script */ string] /* script names */ []string{}
		triggersInterval = map[ /* script name */ string]time.Duration{}
	)
	for _, sconf := range conf.Scripts {
		for _, trigger := range sconf.Triggers {
			switch typ, value, _ := strings.Cut(trigger, " "); typ {
			case "on-start":
				triggersOnStart[sconf.Name] = struct{}{}
			case "pre":
				triggersPrefix[value] = append(triggersPrefix[value], sconf.Name)
			case "script":
				triggersScript[value] = append(triggersScript[value], sconf.Name)
			case "interval":
				triggersInterval[sconf.Name], err = time.ParseDuration(value)
				if err != nil {
					quitErr = fmt.Errorf("parse %q: parse duration: %w", sconf.Name, err)
					return
				}
			default:
				quitErr = fmt.Errorf("parse %q: unknown trigger type %q", sconf.Name, typ)
				return
			}
		}
	}

	slog.Info("loaded triggers",
		"on_start", slices.Collect(maps.Keys(triggersOnStart)),
		"prefix", triggersPrefix,
		"script", triggersScript,
		"interval", triggersInterval,
	)

	vx, err := vaxis.New(vaxis.Options{})
	if err != nil {
		quitErr = err
		return
	}
	defer vx.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// elements
	spinner := newSpinner(vx, 125*time.Millisecond, "▌▀▐▄")
	previewSpinner := newSpinner(vx, 125*time.Millisecond, "▌▀▐▄")

	for scriptName := range triggersOnStart {
		sconf := scripts[scriptName]

		spinner.start()
		go func() {
			defer spinner.stop()

			if err := loadScript(ctx, vx, nil, sconf, ""); err != nil {
				vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
				return
			}
		}()
	}

	// each interval-triggered script reloads itself on its own ticker, so
	// the event loop doesn't need to drive periodic reloads
	for scriptName, inter := range triggersInterval {
		sc := scripts[scriptName]
		go func() {
			ticker := time.NewTicker(inter)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sc.mu.Lock()
					loaded := !sc.lastLoaded.IsZero()
					query := sc.lastQuery
					sc.mu.Unlock()
					if !loaded {
						continue
					}
					if err := loadScript(ctx, vx, nil, sc, query); err != nil {
						vx.PostEvent(eventQuitError(err))
						return
					}
				}
			}
		}()
	}

	input := textinput.
		New().
		SetPrompt("> ")
	input.Prompt = vaxis.Style{Foreground: vaxis.ColorBlack}

	const scriptQueryDebounce = 150 * time.Millisecond
	var lastScriptQuery string
	var scriptQueryChangedAt time.Time

	const previewDebounce = 150 * time.Millisecond
	type previewKey struct {
		sc     *script
		line   string
		loaded time.Time
	}
	var lastPreviewKey previewKey
	var previewTimer *time.Timer
	var imgState imageState

	var index int
	var selectedScripts []string

	type line struct {
		script, text string
		style        lineStyle
	}

	var visScripts []string
	var visLines []line

	active := func() (*script, line, bool) {
		if index < 0 || index >= len(visLines) || visLines[index].style.label {
			return nil, line{}, false
		}
		item := visLines[index]
		return scripts[item.script], item, true
	}

	// step returns the next non-label line from `from` in direction `dir`, or `from` if there is none
	step := func(lines []line, from, dir int) int {
		for i := from + dir; i >= 0 && i < len(lines); i += dir {
			if !lines[i].style.label {
				return i
			}
		}
		return from
	}

	stepGroup := func(lines []line, from, dir int) int {
		cur := from
		for cur >= 0 && cur < len(lines) && lines[cur].script == lines[from].script {
			cur += dir
		}
		if cur < 0 || cur >= len(lines) {
			return from
		}
		for cur > 0 && lines[cur-1].script == lines[cur].script {
			cur--
		}
		return cur
	}

	for ev := range vx.Events() {
		win := vx.Window()
		win.Clear()

		width, height := win.Size()

		input.Update(ev)
		scriptQuery, filterQuery := parseInput(input.String())

		switch ev := ev.(type) {
		case vaxis.Key:
			switch ev.String() {
			case "Escape", "Ctrl+c":
				return
			case "Down":
				index = step(visLines, index, +1)
			case "Up":
				index = step(visLines, index, -1)
			case "Shift+Down":
				index = stepGroup(visLines, index, +1)
			case "Shift+Up":
				index = stepGroup(visLines, index, -1)
			case "End":
			case "Home":
			case "Page_Down":
			case "Page_Up":
			case "Ctrl+r":
				sconf, _, ok := active()
				if !ok {
					break
				}
				sq := scriptQuery
				go func() {
					if err := loadScript(ctx, vx, spinner, sconf, sq); err != nil {
						vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
						return
					}
				}()
			case "Enter", "Shift+Enter":
				sconf, ln, ok := active()
				if !ok {
					break
				}
				stay := ln.style.stay || sconf.StayOpen || ev.Modifiers&vaxis.ModShift != 0
				sq := scriptQuery
				go func() {
					if err := execScript(ctx, spinner, sconf, sq, ln.text); err != nil {
						vx.PostEvent(quitErrorf("run script item for %q: %w", sconf.Name, err))
						return
					}
					if !stay {
						vx.PostEvent(vaxis.QuitEvent{})
						return
					}
					if err := loadScript(ctx, vx, spinner, sconf, sq); err != nil {
						vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
						return
					}
					for _, scriptName := range triggersScript[sconf.Name] {
						if err := loadScript(ctx, vx, spinner, scripts[scriptName], sq); err != nil {
							vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
							return
						}
					}
				}()
			}
		case vaxis.QuitEvent:
			return
		case eventQuitError:
			quitErr = ev
			return
		case vaxis.SyncFunc:
			ev()
		case vaxis.Redraw:
			imgState.settle()
		}

		if scriptQuery != lastScriptQuery {
			lastScriptQuery = scriptQuery
			scriptQueryChangedAt = time.Now()
			for _, scriptName := range selectedScripts {
				scripts[scriptName].load.abort(scriptQuery)
			}
		}
		reloadScripts := !scriptQueryChangedAt.IsZero() && time.Since(scriptQueryChangedAt) >= scriptQueryDebounce
		if reloadScripts {
			scriptQueryChangedAt = time.Time{}
		}

		selectedScripts = selectedScripts[:0]

		// add prefix triggers
		left, after, _ := strings.Cut(filterQuery, " ")
		if scriptNames := triggersPrefix[left]; len(scriptNames) > 0 {
			selectedScripts = append(selectedScripts, scriptNames...)
			filterQuery = after
			// add script triggers
			for _, scriptName := range selectedScripts {
				selectedScripts = append(selectedScripts, triggersScript[scriptName]...)
			}
		}

		// fallback to start scripts
		if len(selectedScripts) == 0 {
			for _, scriptName := range scriptOrder {
				if _, ok := triggersOnStart[scriptName]; ok {
					selectedScripts = append(selectedScripts, scriptName)
				}
			}
		}

		// invoke scripts that haven't been run yet, or reload after script query changes
		for _, scriptName := range selectedScripts {
			script := scripts[scriptName]
			if !script.lastLoaded.IsZero() && !reloadScripts {
				continue
			}
			sq := scriptQuery
			go func() {
				if err := loadScript(ctx, vx, spinner, script, sq); err != nil {
					vx.PostEvent(eventQuitError(err))
					return
				}
			}()
		}

		visLines = visLines[:0]
		visScripts = visScripts[:0]

		for _, scriptName := range selectedScripts {
			script := scripts[scriptName]

			var scriptVisible bool
			for _, item := range script.lines {
				text, style := parseLineStyle(item)
				if filterQuery == "" || match(text, filterQuery) {
					visLines = append(visLines, line{script: scriptName, text: text, style: style})
					scriptVisible = true
				}
			}
			if scriptVisible {
				visScripts = append(visScripts, scriptName)
			}
		}

		// keep cursor off labels
		index = clamp(index, 0, len(visLines)-1)
		if index >= 0 && visLines[index].style.label {
			if n := step(visLines, index, +1); n != index {
				index = n
			} else {
				index = step(visLines, index, -1)
			}
		}

		var previewSc *script
		var previewLine string
		if sc, ln, ok := active(); ok && sc.Preview {
			previewSc = sc
			previewLine = ln.text
		}

		listW := width
		var prevWin vaxis.Window
		if previewSc != nil {
			listW = width / 2
			prevWin = win.New(listW+1, 1, width-listW-1, height-2)
		}

		var key previewKey
		if previewSc != nil {
			key = previewKey{previewSc, previewLine, previewSc.lastLoaded}
		}
		if key != lastPreviewKey {
			if lastPreviewKey.sc != nil {
				lastPreviewKey.sc.preview.abort(key.line)
			}
			lastPreviewKey = key
			if previewTimer != nil {
				previewTimer.Stop()
			}
			if previewSc != nil {
				sc, line, sq := previewSc, previewLine, scriptQuery
				cols, rows := prevWin.Size()
				previewTimer = time.AfterFunc(previewDebounce, func() {
					if err := previewScript(ctx, vx, previewSpinner, sc, sq, line, cols, rows); err != nil {
						vx.PostEvent(quitErrorf("preview script %q: %w", sc.Name, err))
					}
				})
			}
		}

		inpWin := win.New(0, 0, width, 1)
		input.Draw(inpWin)

		spinWin := win.New(0, 0, 1, 1)
		spinner.draw(spinWin)

		listWin := win.New(0, 1, listW, height-2)
		for i, it := range visLines {
			drawLine(listWin, i, scripts[it.script], it.text, it.style, i == index && !it.style.label)
		}

		if previewSc != nil {
			div := win.New(listW, 1, 1, height-2)
			div.Fill(vaxis.Cell{Character: vaxis.Character{Grapheme: "│", Width: 1}, Style: vaxis.Style{Foreground: vaxis.ColorBlack}})

			previewSc.mu.Lock()
			pv := previewSc.previewResult
			ready := previewSc.previewLine == previewLine
			previewSc.mu.Unlock()

			if pv != nil && ready {
				imgState.draw(prevWin, vx, pv)
			} else {
				imgState.destroy()
				previewSpinner.draw(prevWin.New(0, 0, 1, 1))
			}
		} else {
			imgState.destroy()
		}

		footerWin := win.New(0, height-1, width, 1)
		drawFooter(footerWin, conf, visScripts)

		vx.Render()
	}
}

type eventQuitError error

func quitErrorf(f string, a ...any) error {
	return eventQuitError(fmt.Errorf(f, a...))
}

func drawLine(win vaxis.Window, i int, script *script, text string, ls lineStyle, selected bool) {
	if len(script.Columns) > 0 {
		columns := strings.Split(text, "\t")
		filtered := make([]string, 0, len(columns))
		for _, c := range script.Columns { // 1 indexed display columns
			if i := c - 1; i <= len(columns)-1 {
				filtered = append(filtered, columns[i])
			}
		}
		text = strings.Join(filtered, " ")
	} else {
		text = strings.ReplaceAll(text, "\t", " ")
	}

	var col string = "▌"
	if ls.highlight {
		col = "█"
	}

	var style vaxis.Style
	if selected {
		style.Attribute |= vaxis.AttrReverse
	}
	if ls.highlight {
		style.Attribute |= vaxis.AttrBold
	}
	if ls.label {
		style.Attribute |= vaxis.AttrDim
	}

	win.Println(i,
		vaxis.Segment{Text: padRight(script.Name, " ", 13)},
		vaxis.Segment{Text: col, Style: vaxis.Style{Foreground: vaxis.IndexColor(uint8(script.Colour))}},
		vaxis.Segment{Text: " "},
		vaxis.Segment{Text: text, Style: style},
	)
}

type preview struct {
	text string
	img  image.Image
}

func parsePreview(out []byte) (*preview, error) {
	kind, payload, _, ok := cutOSC(string(out))
	if !ok {
		return &preview{text: string(out)}, nil
	}

	var r io.Reader
	switch kind {
	case markerImageData:
		r = base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload))
	case markerImagePath:
		f, err := os.Open(payload)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	default:
		return &preview{text: string(out)}, nil
	}

	img, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}
	return &preview{img: img}, nil
}

// imageState double-buffers preview images so swaps never blank: the old image
// stays drawn under the new one until it finishes encoding (a Redraw), see settle.
type imageState struct {
	src      *preview
	cur, old vaxis.Image
}

func (st *imageState) draw(win vaxis.Window, vx *vaxis.Vaxis, pv *preview) {
	if pv != nil && pv != st.src {
		st.src = pv
		if st.old != nil {
			st.old.Destroy()
		}
		st.old = nil
		if pv.img != nil {
			st.old, st.cur = st.cur, nil
			if img, err := vx.NewImage(pv.img); err == nil {
				cols, rows := win.Size()
				img.Resize(cols, rows)
				st.cur = img
			}
		} else if st.cur != nil {
			st.cur.Destroy()
			st.cur = nil
		}
	}

	if st.cur != nil || st.old != nil {
		if st.old != nil {
			st.old.Draw(win)
		}
		if st.cur != nil {
			st.cur.Draw(win)
		}
	} else if pv != nil {
		win.Print(styledSegments(vx, pv.text)...)
	}
}

// the new image finished encoding and will place this render, so drop the previous one we were holding underneath it
func (st *imageState) settle() {
	if st.old != nil {
		st.old.Destroy()
		st.old = nil
	}
}

func (st *imageState) destroy() {
	if st.cur != nil {
		st.cur.Destroy()
	}
	if st.old != nil {
		st.old.Destroy()
	}
	*st = imageState{}
}

func styledSegments(vx *vaxis.Vaxis, s string) []vaxis.Segment {
	cells := vx.NewStyledString(s, vaxis.Style{}).Cells
	segs := make([]vaxis.Segment, len(cells))
	for i, c := range cells {
		segs[i] = vaxis.Segment{Text: c.Grapheme, Style: c.Style}
	}
	return segs
}

func drawFooter(win vaxis.Window, conf config, visScripts []string) {
	footSegs := make([]vaxis.Segment, 0, len(conf.Scripts)*2)
	footSegs = append(footSegs, vaxis.Segment{Text: "# ", Style: vaxis.Style{Foreground: vaxis.ColorBlack}})

	for _, sconf := range conf.Scripts {
		if len(footSegs) > 1 {
			footSegs = append(footSegs, vaxis.Segment{Text: " "})
		}
		var style = vaxis.Style{Foreground: vaxis.ColorBlack}
		if slices.Contains(visScripts, sconf.Name) {
			style = vaxis.Style{UnderlineStyle: vaxis.UnderlineSingle}
		}
		footSegs = append(footSegs, vaxis.Segment{Text: sconf.Name, Style: style})
	}

	win.Println(0, footSegs...)
}

type script struct {
	scriptConf
	mu         sync.Mutex
	load       taskSlot
	preview    taskSlot
	executing  atomic.Bool
	lastLoaded time.Time
	lastQuery  string
	lines      []string

	previewResult *preview
	previewLine   string
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, sc *script, query string) error {
	ctx, gen, ok := sc.load.take(ctx, query)
	if !ok {
		return nil
	}
	defer sc.load.release(gen)

	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	start := time.Now()

	cmd := makeCmd(ctx, sc, modeList, query, "")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var lines []string
	bs := bufio.NewScanner(stdout)
	for bs.Scan() {
		lines = append(lines, bs.Text())
	}
	if err := bs.Err(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	slog.InfoContext(ctx, "loaded script", "script", sc.Name, "num_lines", len(lines), "took_ms", time.Since(start).Milliseconds())

	vx.SyncFunc(func() {
		if !sc.load.current(gen) {
			return
		}
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if len(lines) > 0 {
			sc.lines = lines
		}
		sc.lastLoaded = time.Now()
		sc.lastQuery = query
	})

	return nil
}

func execScript(parent context.Context, spinner *spinner, sc *script, query, text string) error {
	if !sc.executing.CompareAndSwap(false, true) {
		return nil
	}
	defer sc.executing.Store(false)

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	return makeCmd(ctx, sc, modeRun, query, text).Run()
}

func previewScript(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, sc *script, query, line string, cols, rows int) error {
	ctx, gen, ok := sc.preview.take(ctx, line)
	if !ok {
		return nil
	}
	defer sc.preview.release(gen)

	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	out, err := makeCmd(ctx, sc, modePreview, query, line,
		fmt.Sprintf("CMENU_PREVIEW_COLS=%d", cols),
		fmt.Sprintf("CMENU_PREVIEW_LINES=%d", rows),
	).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	pv, err := parsePreview(out)
	if err != nil {
		return err
	}

	vx.SyncFunc(func() {
		if !sc.preview.current(gen) {
			return
		}
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.previewResult = pv
		sc.previewLine = line
	})

	return nil
}

const (
	modeList    = "list"
	modeRun     = "run"
	modePreview = "preview"
)

func makeCmd(ctx context.Context, sc *script, mode, query, line string, extraEnv ...string) *exec.Cmd {
	var args []string
	if line != "" {
		args = append(args, line)
	}
	cmd := exec.CommandContext(ctx, sc.Path, args...)
	cmd.Env = append(cmd.Environ(), "CMENU_MODE="+mode, "CMENU_INPUT="+query)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd
}

func clamp[T cmp.Ordered](v, mn, mx T) T {
	v = max(v, mn)
	v = min(v, mx)
	return v
}

// parseInput splits input like "cc [1+3] 4" into scriptQuery "1+3" and filterQuery "cc 4"
func parseInput(s string) (scriptQuery, filterQuery string) {
	open := strings.Index(s, "[")
	if open < 0 {
		return "", s
	}
	cl := strings.Index(s[open:], "]")
	if cl < 0 {
		return "", s
	}
	cl += open
	scriptQuery = s[open+1 : cl]
	filterQuery = strings.Join(strings.Fields(s[:open]+" "+s[cl+1:]), " ")
	return scriptQuery, filterQuery
}

func match(text, s string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(s))
}

type lineStyle struct {
	highlight bool
	stay      bool
	label     bool
}

// escape code is 6366, or the first 4 numbers of ASCII "cmenu" in hex
const oscPrefix = "\x1b]6366;"
const oscTerm = "\x07"

// marker kinds shared between the emit subcommands in main and the parsers
const (
	markerHighlight = "highlight"
	markerStay      = "stay"
	markerLabel     = "label"
	markerImageData = "image-data"
	markerImagePath = "image-path"
)

func cutOSC(s string) (kind, payload, rest string, ok bool) {
	after, ok := strings.CutPrefix(s, oscPrefix)
	if !ok {
		return "", "", s, false
	}
	body, rest, ok := strings.Cut(after, oscTerm)
	if !ok {
		return "", "", s, false
	}
	kind, payload, _ = strings.Cut(body, ";")
	return kind, payload, rest, true
}

func parseLineStyle(raw string) (text string, style lineStyle) {
	text = raw
	for {
		kind, _, rest, ok := cutOSC(text)
		if !ok {
			break
		}
		text = rest
		switch kind {
		case markerHighlight:
			style.highlight = true
		case markerStay:
			style.stay = true
		case markerLabel:
			style.label = true
		}
	}
	return text, style
}

type spinner struct {
	model *vxspinner.Model
	count atomic.Int32
}

func newSpinner(vx *vaxis.Vaxis, duration time.Duration, frames string) *spinner {
	model := vxspinner.New(vx, duration)
	model.Frames = []rune(frames)
	return &spinner{
		model: model,
	}
}

func (s *spinner) start() {
	if s.count.Add(1) == 1 {
		s.model.Start()
	}
}

func (s *spinner) stop() {
	if s.count.Add(-1) == 0 {
		s.model.Stop()
	}
}

func (s *spinner) draw(w vaxis.Window) {
	s.model.Draw(w)
}

// avoiding fmt.Sprintf in a hot loop
func padRight(s string, p string, width int) string {
	gap := width - len(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(p, gap)
}

type config struct {
	Scripts []scriptConf `toml:"scripts"`
}

type scriptConf struct {
	Triggers []string `toml:"triggers"`
	Name     string   `toml:"name"`
	Path     string   `toml:"path"`
	Colour   int      `toml:"colour"`
	Columns  []int    `toml:"columns"`
	StayOpen bool     `toml:"stay_open"`
	Preview  bool     `toml:"preview"`
}

func parseConfig(path string) (config, error) {
	configFile, err := os.Open(path)
	if err != nil {
		return config{}, err
	}

	var conf config
	if _, err := toml.NewDecoder(configFile).Decode(&conf); err != nil {
		return config{}, err
	}

	return conf, nil
}

// taskSlot runs at most one task at a time. take starts a new task: if a task with the
// same key is already running, it returns ok=false. if a task with a different key is
// running, that task's context is cancelled
type taskSlot struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	key    string
	gen    uint64
}

func (t *taskSlot) take(ctx context.Context, key string) (context.Context, uint64, bool) {
	t.mu.Lock()
	if t.cancel != nil && t.key == key {
		t.mu.Unlock()
		return nil, 0, false
	}
	prev := t.cancel
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.gen++
	gen := t.gen
	t.key = key
	t.mu.Unlock()

	if prev != nil {
		prev()
	}
	return ctx, gen, true
}

func (t *taskSlot) abort(key string) {
	t.mu.Lock()
	var cancel context.CancelFunc
	if t.cancel != nil && t.key != key {
		cancel = t.cancel
	}
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (t *taskSlot) release(gen uint64) {
	var cancel context.CancelFunc
	t.mu.Lock()
	if t.gen == gen {
		cancel = t.cancel
		t.cancel = nil
		t.key = ""
	}
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (t *taskSlot) current(gen uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen == gen
}
