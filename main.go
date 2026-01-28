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
	conf, err := parseConfig(filepath.Join(configDir, "cmenu", "config.toml"))
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
		triggersInput    = map[ /* script name */ string]time.Duration{}
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
			case "input":
				var delay = 100 * time.Millisecond
				if value != "" {
					delay, err = time.ParseDuration(value)
					if err != nil {
						quitErr = fmt.Errorf("parse %q: parse duration: %w", sconf.Name, err)
						return
					}
				}
				triggersInput[sconf.Name] = delay
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
		"input", slices.Collect(maps.Keys(triggersInput)),
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

	inp := textinput.
		New().
		SetPrompt("> ")
	inp.Prompt = vaxis.Style{Foreground: vaxis.ColorBlack}

	for scriptName := range triggersOnStart {
		sconf := scripts[scriptName]

		spinner.start()
		go func() {
			defer spinner.stop()

			if err := runScript(ctx, vx, nil, sconf, ""); err != nil {
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

	// state
	type line struct{ script, text string }
	var (
		index              int
		query              string
		queryChangedAt     time.Time
		scriptQuery        string
		selectedScripts    []string
		visScripts         []string
		visLines           []line
		inputTriggerCtx    context.Context
		inputTriggerCancel func()
	)

	active := func() (int, *script, string) {
		item := visLines[index]
		sconf := scripts[item.script]
		return index, sconf, item.text
	}

	for ev := range vx.Events() {
		win := vx.Window()
		win.Clear()

		width, height := win.Size()

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
			case "Right":
				_, sconf, _ := active()
				go func() {
					if err := runScript(ctx, vx, spinner, sconf, scriptQuery); err != nil {
						vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
						return
					}
				}()
			case "Enter", "Shift+Enter":
				_, sconf, text := active()
				text, _ = parseLineStyle(text)
				stayOpen := sconf.StayOpen || ev.Modifiers&vaxis.ModShift != 0
				_, isInputTrigger := triggersInput[sconf.Name]
				go func() {
					if err := runScript(ctx, vx, spinner, sconf, scriptQuery, text); err != nil {
						vx.PostEvent(quitErrorf("run script item for %q: %w", sconf.Name, err))
						return
					}
					if !stayOpen {
						vx.PostEvent(vaxis.QuitEvent{})
						return
					}
					if isInputTrigger {
						return // input scripts wait for new input, don't auto-reload
					}
					if err := runScript(ctx, vx, spinner, sconf, scriptQuery); err != nil {
						vx.PostEvent(quitErrorf("load script %q: %w", sconf.Name, err))
						return
					}
					for _, scriptName := range triggersScript[sconf.Name] {
						if err := runScript(ctx, vx, spinner, scripts[scriptName], scriptQuery); err != nil {
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

		inp.Update(ev)
		prevQuery := query
		query = inp.String()

		selectedScripts = selectedScripts[:0]
		filterQuery := query
		scriptQuery = query

		// add prefix triggers
		if left, rest, ok := strings.Cut(filterQuery, " "); ok {
			if scriptNames := triggersPrefix[left]; len(scriptNames) > 0 {
				selectedScripts = append(selectedScripts, scriptNames...)
				filterQuery = rest
				scriptQuery = rest
			}
			// add script triggers
			for _, scriptName := range selectedScripts {
				selectedScripts = append(selectedScripts, triggersScript[scriptName]...)
			}
		}

		if query != prevQuery {
			queryChangedAt = time.Now()
		}

		// cancel any running input scripts and reset debounce
		if query != prevQuery {
			if inputTriggerCancel != nil {
				inputTriggerCancel()
			}
			inputTriggerCtx, inputTriggerCancel = context.WithCancel(ctx)
		}

		// fallback to start scripts
		if len(selectedScripts) == 0 {
			for _, scriptName := range scriptOrder {
				if _, ok := triggersOnStart[scriptName]; ok {
					selectedScripts = append(selectedScripts, scriptName)
				}
			}
		}

		// invoke scripts that haven't been run yet (input scripts wait for debounce)
		for _, scriptName := range selectedScripts {
			if _, isInputTrigger := triggersInput[scriptName]; isInputTrigger {
				continue
			}
			if script := scripts[scriptName]; len(script.lines) == 0 {
				go func() {
					if err := runScript(ctx, vx, spinner, script, scriptQuery); err != nil {
						vx.PostEvent(eventQuitError(err))
						return
					}
				}()
			}
		}

		visLines = visLines[:0]
		visScripts = visScripts[:0]

		for _, scriptName := range selectedScripts {
			script := scripts[scriptName]
			_, isTriggerInput := triggersInput[scriptName]

			var scriptVisible bool
			for _, item := range script.lines {
				if query == "" || isTriggerInput || match(item, filterQuery) {
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
				go func() {
					if err := runScript(ctx, vx, nil, script, scriptQuery); err != nil {
						vx.PostEvent(eventQuitError(err))
						return
					}
				}()
			}
		}

		// run input triggers after debounce
		for _, scriptName := range selectedScripts {
			delay, ok := triggersInput[scriptName]
			if !ok {
				continue
			}
			if queryChangedAt.IsZero() || time.Since(queryChangedAt) < delay {
				continue
			}
			queryChangedAt = time.Time{}

			scriptQuery, inputTriggerCtx := scriptQuery, inputTriggerCtx
			go func() {
				if err := runScript(inputTriggerCtx, vx, spinner, scripts[scriptName], scriptQuery); err != nil && inputTriggerCtx.Err() == nil {
					vx.PostEvent(eventQuitError(err))
					return
				}
			}()
		}

		inpWin := win.New(0, 0, width, 1)
		inp.Draw(inpWin)

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
	running    bool
	lastLoaded time.Time
	lines      []string
}

func runScript(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, script *script, query string, args ...string) error {
	start := time.Now()

	script.mu.Lock()
	if script.running {
		script.mu.Unlock()
		return nil
	}
	script.running = true
	script.mu.Unlock()

	defer func() {
		script.mu.Lock()
		script.running = false
		script.mu.Unlock()
	}()

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, script.Path, args...)
	cmd.Env = append(cmd.Environ(),
		"CMENU_QUERY="+query,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 100 * time.Millisecond

	if len(args) > 0 {
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	var lines []string

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return err
	}

	slog.InfoContext(ctx, "loaded script", "script", script.Name, "num_lines", len(lines), "took_ms", time.Since(start).Milliseconds())

	vx.SyncFunc(func() {
		script.mu.Lock()
		if len(lines) > 0 {
			script.lines = lines
		}
		script.lastLoaded = time.Now()
		script.mu.Unlock()
	})

	return nil
}

func clamp[T cmp.Ordered](v, mn, mx T) T {
	v = max(v, mn)
	v = min(v, mx)
	return v
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
