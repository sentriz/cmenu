package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
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

	configDir, _ := os.UserConfigDir()
	confPath := filepath.Join(configDir, "cmenu", "config.toml")
	if len(os.Args) == 2 {
		confPath = os.Args[1]
	}

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
		triggersScript   = map[ /* script name */ string] /* script names */ []string{}
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
				triggersScript[sconf.Name] = append(triggersScript[sconf.Name], value)
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

	// periodic redraws to check intervals
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				vx.PostEvent(vaxis.Redraw{})
			}
		}
	}()

	input := textinput.
		New().
		SetPrompt("> ")
	input.Prompt = vaxis.Style{Foreground: vaxis.ColorBlack}

	const scriptQueryDebounce = 150 * time.Millisecond
	var lastScriptQuery string
	var scriptQueryChangedAt time.Time

	var index int
	var selectedScripts []string

	type line struct{ script, text string }

	var visScripts []string
	var visLines []line

	active := func() (int, *script, string) {
		item := visLines[index]
		sconf := scripts[item.script]
		return index, sconf, item.text
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
				index = clamp(index+1, 0, len(visLines)-1)
			case "Up":
				index = clamp(index-1, 0, len(visLines)-1)
			case "End":
			case "Home":
			case "Page_Down":
			case "Page_Up":
			case "Ctrl+r":
				_, sconf, _ := active()
				sq := scriptQuery
				go func() {
					if err := loadScript(ctx, vx, spinner, sconf, sq); err != nil {
						vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
						return
					}
				}()
			case "Enter", "Shift+Enter":
				_, sconf, text := active()
				text, _ = parseLineStyle(text)
				stayOpen := sconf.StayOpen || ev.Modifiers&vaxis.ModShift != 0
				sq := scriptQuery
				go func() {
					if err := execScript(ctx, spinner, sconf, sq, text); err != nil {
						vx.PostEvent(quitErrorf("run script item for %q: %w", sconf.Name, err))
						return
					}
					if !stayOpen {
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
		}

		if scriptQuery != lastScriptQuery {
			lastScriptQuery = scriptQuery
			scriptQueryChangedAt = time.Now()
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
				if filterQuery == "" || match(item, filterQuery) {
					visLines = append(visLines, line{script: scriptName, text: item})
					scriptVisible = true
				}
			}
			if scriptVisible {
				visScripts = append(visScripts, scriptName)
			}
		}

		// run interval triggers
		for _, scriptName := range visScripts {
			inter := triggersInterval[scriptName]
			if inter == 0 {
				continue
			}

			script := scripts[scriptName]

			script.mu.Lock()
			lastLoaded := script.lastLoaded
			script.mu.Unlock()

			if !lastLoaded.IsZero() && time.Since(lastLoaded) >= inter {
				sq := scriptQuery
				go func() {
					if err := loadScript(ctx, vx, nil, script, sq); err != nil {
						vx.PostEvent(eventQuitError(err))
						return
					}
				}()
			}
		}

		inpWin := win.New(0, 0, width, 1)
		input.Draw(inpWin)

		spinWin := win.New(0, 0, 1, 1)
		spinner.draw(spinWin)

		listWin := win.New(0, 1, width, height-2)
		for i, it := range visLines {
			drawLine(listWin, i, scripts[it.script], it.text, i == index)
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

func drawLine(win vaxis.Window, i int, script *script, text string, selected bool) {
	text, lineStyle := parseLineStyle(text)

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
	if lineStyle.highlight {
		col = "█"
	}

	var style vaxis.Style
	if selected {
		style.Attribute |= vaxis.AttrReverse
	}
	if lineStyle.highlight {
		style.Attribute |= vaxis.AttrBold
	}

	win.Println(i,
		vaxis.Segment{Text: padRight(script.Name, " ", 10)},
		vaxis.Segment{Text: col, Style: vaxis.Style{Foreground: vaxis.IndexColor(uint8(script.Colour))}},
		vaxis.Segment{Text: " "},
		vaxis.Segment{Text: text, Style: style},
	)
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
	executing  atomic.Bool
	lastLoaded time.Time
	lines      []string
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

	cmd := makeCmd(ctx, sc, query)
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
	return makeCmd(ctx, sc, query, text).Run()
}

func makeCmd(ctx context.Context, sc *script, query string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, sc.Path, args...)
	cmd.Env = append(cmd.Environ(), "CMENU_QUERY="+query)
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

func match(str, s string) bool {
	str, _ = parseLineStyle(str)
	str = strings.ToLower(str)
	s = strings.ToLower(s)
	return strings.Contains(str, s)
}

type lineStyle struct {
	highlight bool
}

// escape code is 6366, or the first 4 numbers of ASCII "cmenu" in hex
const oscPrefix = "\x1b]6366;"

func parseLineStyle(raw string) (text string, style lineStyle) {
	text = raw
	for strings.HasPrefix(text, oscPrefix) {
		end := strings.Index(text, "\x07")
		if end == -1 {
			break
		}
		option := text[len(oscPrefix):end]
		text = text[end+1:]
		switch option {
		case "highlight":
			style.highlight = true
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
	StayOpen bool     `toml:"stay_open"`
	Columns  []int    `toml:"columns"`
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
