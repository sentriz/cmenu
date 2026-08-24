package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/BurntSushi/toml"
	"go.rockorager.dev/vaxis"
	vxspinner "go.rockorager.dev/vaxis/widgets/spinner"
	"go.rockorager.dev/vaxis/widgets/textinput"
)

func main() {
	// may be overwritten later by log file
	var slogWriter io.Writer = &bytes.Buffer{}

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

	if logPath := os.Getenv("CMENU_LOG_PATH"); logPath != "" {
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			quitErr = err
			return
		}
		slogWriter = logFile
	}
	{
		slogHandler := slog.NewJSONHandler(slogWriter, nil)
		slogLogger := slog.New(slogHandler)
		slog.SetDefault(slogLogger)
	}

	configDir, _ := os.UserConfigDir()
	confPath := filepath.Join(configDir, "cmenu", "config.toml")

	conf, err := parseConfig(confPath)
	if err != nil {
		quitErr = err
		return
	}

	const defaultDebounce = 150 * time.Millisecond

	var scripts = map[string]*script{}
	var scriptOrder = make([]string, 0, len(conf.Scripts))
	for _, sconf := range conf.Scripts {
		sc := &script{scriptConf: sconf, debounce: defaultDebounce}
		if sconf.Debounce != "" {
			sc.debounce, err = time.ParseDuration(sconf.Debounce)
			if err != nil {
				quitErr = fmt.Errorf("parse %q: parse debounce: %w", sconf.Name, err)
				return
			}
		}
		scripts[sconf.Name] = sc
		scriptOrder = append(scriptOrder, sconf.Name)
	}

	var (
		triggersOnStart  [] /* script names */ string
		triggersPrefix   = map[ /* prefix */ string] /* script names */ []string{}
		triggersScript   = map[ /* script */ string] /* script names */ []string{}
		triggersInterval = map[ /* script name */ string]time.Duration{}
	)
	for _, sconf := range conf.Scripts {
		for _, trigger := range sconf.Triggers {
			switch typ, value, _ := strings.Cut(trigger, " "); typ {
			case "on-start":
				triggersOnStart = append(triggersOnStart, sconf.Name)
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
		"on_start", triggersOnStart,
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

	const previewDebounce = 150 * time.Millisecond
	for _, scriptName := range scriptOrder {
		sc := scripts[scriptName]
		sc.loads = make(chan loadReq, 1)
		go worker(ctx, vx, sc.loads, sc.debounce, func(ctx context.Context, req loadReq) error {
			return runLoad(ctx, vx, spinner, sc, req)
		})
		if sc.Preview {
			sc.previews = make(chan previewReq, 1)
			go worker(ctx, vx, sc.previews, previewDebounce, func(ctx context.Context, req previewReq) error {
				return runPreview(ctx, vx, previewSpinner, sc, req)
			})
		}
	}

	for _, scriptName := range triggersOnStart {
		requestLoad(scripts[scriptName], "")
	}

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
					vx.PostEvent(eventInterval{sc: sc})
				}
			}
		}()
	}

	input := textinput.
		New().
		SetPrompt("> ")
	input.Prompt = vaxis.Style{Attribute: vaxis.AttrDim}

	type previewKey struct {
		sc     *script
		line   string
		loaded time.Time
	}
	var lastPreviewKey previewKey
	var imgState imageState

	var index int
	var scroll int
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
		if rs, ok := ev.(vaxis.Resize); ok {
			vx.Resize(rs)
		}

		win := vx.Window()
		win.Clear()

		width, height := win.Size()
		rows := height - 2

		input.Update(ev)
		if key, ok := ev.(vaxis.Key); ok && key.EventType != vaxis.EventRelease {
			autoPair(input, key)
		}
		selectName, selected, scriptQuery, filterQuery := parseInput(input.String())

		switch ev := ev.(type) {
		case vaxis.Key:
			if ev.EventType == vaxis.EventRelease {
				break
			}
			switch ev.String() {
			case "Escape", "Ctrl+c", "Ctrl+d":
				return
			case "Down":
				index = step(visLines, index, +1)
			case "Up":
				index = step(visLines, index, -1)
			case "Shift+Right", "Shift+Left":
				dir := 1
				if ev.String() == "Shift+Left" {
					dir = -1
				}
				cur := selectName
				if left, _, _ := strings.Cut(filterQuery, " "); len(triggersPrefix[left]) > 0 {
					cur = triggersPrefix[left][0]
				}
				input.SetContent(selectPrefix + cycleScript(scriptOrder, cur, dir) + " ")
				selectName, selected, scriptQuery, filterQuery = parseInput(input.String())
				index, scroll = 0, 0
			case "Shift+Down":
				index = stepGroup(visLines, index, +1)
			case "Shift+Up":
				index = stepGroup(visLines, index, -1)
			case "End":
				index = len(visLines) - 1
			case "Home":
				index = 0
			case "Page_Down":
				index = min(index+rows-1, len(visLines)-1)
			case "Page_Up":
				index = max(index-rows+1, 0)
			case "Ctrl+r":
				sc, _, ok := active()
				if !ok {
					break
				}
				requestLoad(sc, scriptQuery)
			case "Enter", "Shift+Enter":
				sc, ln, ok := active()
				if !ok || sc.executing {
					break
				}
				stay := ln.style.stay || sc.StayOpen || ev.Modifiers&vaxis.ModShift != 0
				sc.executing = true
				query, line := scriptQuery, ln.text
				go func() {
					ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()

					spinner.start()
					err := makeCmd(ctx, sc, modeRun, query, line).Run()
					spinner.stop()
					if err != nil {
						vx.PostEvent(quitErrorf("run script item for %q: %w", sc.Name, err))
						return
					}
					vx.PostEvent(eventExecDone{sc: sc, query: query, stay: stay})
				}()
			}
		case vaxis.QuitEvent:
			return
		case eventQuitError:
			quitErr = ev
			return
		case eventLines:
			if len(ev.lines) > 0 {
				ev.sc.lines = ev.lines
			}
			ev.sc.lastLoaded = time.Now()
			ev.sc.lastQuery = ev.query
		case eventPreview:
			ev.sc.previewResult = ev.pv
			ev.sc.previewLine = ev.line
		case eventExecDone:
			ev.sc.executing = false
			if !ev.stay {
				return
			}
			requestLoad(ev.sc, ev.query)
			for _, scriptName := range triggersScript[ev.sc.Name] {
				requestLoad(scripts[scriptName], ev.query)
			}
		case eventInterval:
			if !ev.sc.lastLoaded.IsZero() {
				send(ev.sc.loads, loadReq{query: ev.sc.lastQuery, quiet: true})
			}
		case vaxis.SyncFunc:
			ev()
		case vaxis.Redraw:
			imgState.settle()
		}

		selectedScripts = selectedScripts[:0]

		// selectScripts adds scripts plus everything they trigger
		selectScripts := func(scriptNames ...string) {
			selectedScripts = append(selectedScripts, scriptNames...)
			for _, scriptName := range scriptNames {
				selectedScripts = append(selectedScripts, triggersScript[scriptName]...)
			}
		}

		left, after, _ := strings.Cut(filterQuery, " ")
		switch scriptNames := triggersPrefix[left]; {
		case selected:
			if scripts[selectName] != nil {
				selectScripts(selectName)
			}
		case len(scriptNames) > 0:
			filterQuery = after
			selectScripts(scriptNames...)
		default:
			selectedScripts = append(selectedScripts, triggersOnStart...)
		}

		// invoke scripts that haven't been asked for this query yet, the worker debounces reloads
		for _, scriptName := range selectedScripts {
			script := scripts[scriptName]
			if script.sentQuerySet && script.sentQuery == scriptQuery {
				continue
			}
			requestLoad(script, scriptQuery)
		}

		for fuzz := range 3 {
			visLines = visLines[:0]
			visScripts = visScripts[:0]

			for _, scriptName := range selectedScripts {
				script := scripts[scriptName]

				var scriptVisible bool
				for _, item := range script.lines {
					text, style := parseLineStyle(item)
					if filterQuery == "" || matches(displayText(script, text), filterQuery, fuzz) {
						visLines = append(visLines, line{script: scriptName, text: text, style: style})
						scriptVisible = true
					}
				}
				if scriptVisible {
					visScripts = append(visScripts, scriptName)
				}
			}

			if len(visLines) > 0 {
				break
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

		scroll = clamp(scroll, index-rows+1, index)
		scroll = clamp(scroll, 0, max(0, len(visLines)-rows))

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
			lastPreviewKey = key
			if previewSc != nil {
				cols, rows := prevWin.Size()
				send(previewSc.previews, previewReq{query: scriptQuery, line: previewLine, cols: cols, rows: rows})
			}
		}

		inpWin := win.New(0, 0, width, 1)
		input.Draw(inpWin)

		spinWin := win.New(0, 0, 1, 1)
		spinner.draw(spinWin)

		listWin := win.New(0, 1, listW, rows)
		for i := scroll; i < len(visLines) && i-scroll < rows; i++ {
			it := visLines[i]
			drawLine(listWin, i-scroll, scripts[it.script], it.text, it.style, i == index && !it.style.label)
		}

		if previewSc != nil {
			div := win.New(listW, 1, 1, height-2)
			div.Fill(vaxis.Cell{Character: vaxis.Character{Grapheme: "│", Width: 1}, Style: vaxis.Style{Attribute: vaxis.AttrDim}})

			pv := previewSc.previewResult
			ready := previewSc.previewLine == previewLine

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

type eventLines struct {
	sc    *script
	query string
	lines []string
}

type eventPreview struct {
	sc   *script
	line string
	pv   *preview
}

type eventExecDone struct {
	sc    *script
	query string
	stay  bool
}

type eventInterval struct{ sc *script }

type config struct {
	Scripts []scriptConf `toml:"scripts"`
}

type scriptConf struct {
	Triggers []string `toml:"triggers"`
	Name     string   `toml:"name"`
	Path     string   `toml:"path"`
	Colour   int      `toml:"colour"`
	Debounce string   `toml:"debounce"`
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

type script struct {
	scriptConf
	debounce time.Duration
	loads    chan loadReq
	previews chan previewReq

	executing     bool
	lastLoaded    time.Time
	lastQuery     string
	sentQuery     string
	sentQuerySet  bool
	lines         []string
	previewResult *preview
	previewLine   string
}

// requestLoad debounces only when this is a reload for a changed query, so first
// loads, Ctrl+r, and post-exec reloads run immediately
func requestLoad(sc *script, query string) {
	debounce := sc.sentQuerySet && sc.sentQuery != query
	sc.sentQuery, sc.sentQuerySet = query, true
	send(sc.loads, loadReq{query: query, debounce: debounce})
}

func send[T any](ch chan T, req T) {
	for {
		select {
		case ch <- req:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}

type request interface{ debounced() bool }

func worker[T request](ctx context.Context, vx *vaxis.Vaxis, reqs chan T, debounce time.Duration, run func(context.Context, T) error) {
	var req T
	var have bool
	for {
		if !have {
			select {
			case <-ctx.Done():
				return
			case req = <-reqs:
			}
		}
		have = false

		if req.debounced() {
			timer := time.NewTimer(debounce)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case req = <-reqs:
				timer.Stop()
				have = true
				continue
			case <-timer.C:
			}
		}

		runCtx, cancel := context.WithCancel(ctx)
		errc := make(chan error, 1)
		go func(req T) {
			errc <- run(runCtx, req)
		}(req)

		select {
		case req = <-reqs:
			cancel()
			<-errc
			have = true
		case err := <-errc:
			cancel()
			if err != nil {
				vx.PostEvent(eventQuitError(err))
				return
			}
		case <-ctx.Done():
			cancel()
			<-errc
			return
		}
	}
}

type loadReq struct {
	query    string
	debounce bool
	quiet    bool
}

func (r loadReq) debounced() bool { return r.debounce }

func runLoad(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, sc *script, req loadReq) error {
	if !req.quiet {
		spinner.start()
		defer spinner.stop()
	}

	lines, err := loadScript(ctx, sc, req.query)
	if err != nil {
		return fmt.Errorf("load script %q: %w", sc.Name, err)
	}
	if ctx.Err() == nil {
		vx.PostEvent(eventLines{sc: sc, query: req.query, lines: lines})
	}
	return nil
}

func loadScript(ctx context.Context, sc *script, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()

	cmd := makeCmd(ctx, sc, modeList, query, "")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var lines []string
	bs := bufio.NewScanner(stdout)
	for bs.Scan() {
		lines = append(lines, bs.Text())
	}
	if err := bs.Err(); err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		return nil, err
	}

	slog.InfoContext(ctx, "loaded script", "script", sc.Name, "num_lines", len(lines), "took_ms", time.Since(start).Milliseconds())
	return lines, nil
}

type previewReq struct {
	query, line string
	cols, rows  int
}

func (previewReq) debounced() bool { return true }

func runPreview(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, sc *script, req previewReq) error {
	spinner.start()
	defer spinner.stop()

	pv, err := previewScript(ctx, sc, req)
	if err != nil {
		return fmt.Errorf("preview script %q: %w", sc.Name, err)
	}
	if pv != nil {
		vx.PostEvent(eventPreview{sc: sc, line: req.line, pv: pv})
	}
	return nil
}

func previewScript(ctx context.Context, sc *script, req previewReq) (*preview, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := makeCmd(ctx, sc, modePreview, req.query, req.line,
		fmt.Sprintf("CMENU_PREVIEW_COLS=%d", req.cols),
		fmt.Sprintf("CMENU_PREVIEW_LINES=%d", req.rows),
	).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		return nil, err
	}

	return parsePreview(out)
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

func displayText(script *script, text string) string {
	if len(script.Columns) == 0 {
		return strings.ReplaceAll(text, "\t", " ")
	}
	columns := strings.Split(text, "\t")
	filtered := make([]string, 0, len(columns))
	for _, c := range script.Columns { // 1 indexed display columns
		if i := c - 1; i <= len(columns)-1 {
			filtered = append(filtered, columns[i])
		}
	}
	return strings.Join(filtered, " ")
}

func drawLine(win vaxis.Window, i int, script *script, text string, ls lineStyle, selected bool) {
	text = displayText(script, text)

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

// avoiding fmt.Sprintf in a hot loop
func padRight(s string, p string, width int) string {
	gap := width - len(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(p, gap)
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
	footSegs = append(footSegs, vaxis.Segment{Text: "# ", Style: vaxis.Style{Attribute: vaxis.AttrDim}})

	for _, sconf := range conf.Scripts {
		if len(footSegs) > 1 {
			footSegs = append(footSegs, vaxis.Segment{Text: " "})
		}
		var style = vaxis.Style{Attribute: vaxis.AttrDim}
		if slices.Contains(visScripts, sconf.Name) {
			style = vaxis.Style{UnderlineStyle: vaxis.UnderlineSingle}
		}
		footSegs = append(footSegs, vaxis.Segment{Text: sconf.Name, Style: style})
	}

	win.Println(0, footSegs...)
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

const selectPrefix = "#"

// parseInput splits input like "#calc cc [1+3] 4" into the selected script name "calc",
// scriptQuery "1+3" and filterQuery "cc 4". a leading selectPrefix always selects by name,
// so "#" alone selects nothing rather than falling back to the on-start scripts
func parseInput(s string) (selectName string, selected bool, scriptQuery, filterQuery string) {
	if rest, ok := strings.CutPrefix(s, selectPrefix); ok {
		selected = true
		selectName, s, _ = strings.Cut(rest, " ")
	}
	open := strings.Index(s, "[")
	if open < 0 {
		return selectName, selected, "", s
	}
	cl := strings.Index(s[open:], "]")
	if cl < 0 {
		return selectName, selected, "", s
	}
	cl += open
	scriptQuery = s[open+1 : cl]
	filterQuery = strings.Join(strings.Fields(s[:open]+" "+s[cl+1:]), " ")
	return selectName, selected, scriptQuery, filterQuery
}

func autoPair(input *textinput.Model, key vaxis.Key) {
	chars := input.Characters()
	cursor := input.CursorPosition()

	switch {
	case key.Text == "[":
		chars = slices.Insert(chars, cursor, vaxis.Characters("]")...)
	case key.Text == "]" && cursor < len(chars) && chars[cursor].Grapheme == "]":
		chars = slices.Delete(chars, cursor, cursor+1)
	default:
		return
	}

	var content strings.Builder
	for _, char := range chars {
		content.WriteString(char.Grapheme)
	}

	input.SetContent(content.String())
	// SetContent parks the cursor at the end, and there is no way to place it directly
	for range len(chars) - cursor {
		input.Update(vaxis.Key{Keycode: vaxis.KeyLeft})
	}
}

func cycleScript(order []string, cur string, dir int) string {
	if len(order) == 0 {
		return cur
	}
	i := slices.Index(order, cur)
	if i < 0 && dir < 0 {
		i = 0
	}
	return order[(i+dir+len(order))%len(order)]
}

// matches reports whether every query token appears in text, with increasing tolerance per fuzz level:
// 0 plain substring, 1 windowed subsequence, 2 subsequence with one rune of 4+ rune tokens dropped
func matches(text, query string, fuzz int) bool {
	text = strings.ToLower(text)
	query = strings.ToLower(query)

	for tok := range strings.FieldsSeq(query) {
		if !matchToken(text, tok, fuzz) {
			return false
		}
	}
	return true
}

func matchToken(text, tok string, fuzz int) bool {
	if fuzz == 0 {
		return strings.Contains(text, tok)
	}
	line, runes := []rune(text), []rune(tok)
	if subsequence(line, runes) {
		return true
	}
	if fuzz > 1 && len(runes) >= 4 {
		for i := range runes {
			if subsequence(line, slices.Concat(runes[:i], runes[i+1:])) {
				return true
			}
		}
	}
	return false
}

// subsequence reports whether tok appears in order in line within a window of 2x the token length
func subsequence(line, tok []rune) bool {
	for start := range line {
		i := 0
		for j := start; j < len(line) && j < start+2*len(tok) && i < len(tok); j++ {
			if line[j] == tok[i] {
				i++
			}
		}
		if i == len(tok) {
			return true
		}
	}
	return false
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

func clamp[T cmp.Ordered](v, mn, mx T) T {
	v = max(v, mn)
	v = min(v, mx)
	return v
}
