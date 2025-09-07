package main

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"git.sr.ht/~rockorager/vaxis"
	vxspinner "git.sr.ht/~rockorager/vaxis/widgets/spinner"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func init() {
	logHandler := slog.DiscardHandler
	if ok, _ := strconv.ParseBool(os.Getenv("CMENU_DEBUG")); ok {
		logFile, _ := os.OpenFile("/tmp/cm", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
		logHandler = slog.NewJSONHandler(logFile, nil)
	}
	logger := slog.New(logHandler)
	slog.SetDefault(logger)
}

func main() {
	configDir, _ := os.UserConfigDir()
	conf, err := parseConfig(filepath.Join(configDir, "cmenu", "config.toml"))
	if err != nil {
		panic(err)
	}

	var scriptKeys []string
	var scripts = map[string]*script{}
	var triggers = map[string][]string{}

	for _, sconf := range conf.Scripts {
		scriptKeys = append(scriptKeys, sconf.Name)
		scripts[sconf.Name] = &script{
			scriptConf: sconf,
		}
		for i, key := range sconf.Triggers {
			triggers[key] = slices.Insert(triggers[key], clamp(i, 0, len(triggers[key])), sconf.Name)
		}
	}

	vx, err := vaxis.New(vaxis.Options{})
	if err != nil {
		panic(err)
	}
	defer vx.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// elements
	spinner := newSpinner(vx, 100*time.Millisecond, "▌▀▐▄")

	inp := textinput.
		New().
		SetPrompt("> ")
	inp.Prompt = vaxis.Style{Foreground: vaxis.ColorBlack}

	for _, sc := range scripts {
		if sc.Preview == 0 {
			continue
		}

		spinner.start()
		go func() {
			defer spinner.stop()

			if err := loadScript(ctx, vx, nil, sc); err != nil {
				panic(err)
			}
		}()
	}

	// state
	type line struct{ script, text string }
	var (
		index           int
		selectedScripts []string
		visScripts      []string
		visLines        []line
	)

	active := func() (int, *script, string) {
		item := visLines[index]
		sconf := scripts[item.script]
		return index, sconf, item.text
	}

	siblings := func(sconf *script) []*script {
		var sib []*script
		for _, trigScripts := range triggers {
			if slices.Contains(trigScripts, sconf.Name) {
				for _, trigScript := range trigScripts {
					if trigScript != sconf.Name {
						sib = append(sib, scripts[trigScript])
					}
				}
				break
			}
		}
		return sib
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
					if err := loadScript(ctx, vx, spinner, sconf); err != nil {
						panic(err)
					}
				}()
			case "Enter", "Shift+Enter":
				_, sconf, text := active()
				stayOpen := sconf.StayOpen || ev.Modifiers&vaxis.ModShift != 0
				go func() {
					if err := runScriptItem(ctx, vx, spinner, sconf, text); err != nil {
						panic(err)
					}
					if !stayOpen {
						vx.PostEvent(vaxis.QuitEvent{})
						return
					}
					if err := loadScript(ctx, vx, spinner, sconf); err != nil {
						panic(err)
					}
					for _, sconf := range siblings(sconf) {
						if err := loadScript(ctx, vx, spinner, sconf); err != nil {
							panic(err)
						}
					}
				}()
			}
		case vaxis.QuitEvent:
			return
		case vaxis.SyncFunc:
			ev()
		}

		inp.Update(ev)
		inpString := inp.String()

		query := inpString

		selectedScripts = selectedScripts[:0]
		if left, rest, ok := strings.Cut(query, " "); ok {
			if s := triggers[left]; len(s) > 0 {
				selectedScripts = append(selectedScripts, s...)
				query = rest
			}
		}
		if len(selectedScripts) == 0 {
			for _, s := range scriptKeys {
				selectedScripts = append(selectedScripts, s)
			}
		} else {
			for _, s := range selectedScripts {
				if script := scripts[s]; len(script.lines) == 0 {
					go func() {
						if err := loadScript(ctx, vx, spinner, script); err != nil {
							panic(err)
						}
					}()
				}
			}
		}

		visLines = visLines[:0]
		visScripts = visScripts[:0]

		for _, s := range selectedScripts {
			script := scripts[s]

			var scriptVisible bool
			for i, item := range script.lines {
				if inpString == "" && i >= script.Preview {
					break
				}
				if match(item, query) {
					visLines = append(visLines, line{script: s, text: item})
					scriptVisible = true
				}
			}
			if scriptVisible {
				visScripts = append(visScripts, s)
			}
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

func drawLine(win vaxis.Window, i int, script *script, text string, selected bool) {
	var style vaxis.Style
	if selected {
		style.Attribute = vaxis.AttrReverse
	}

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

	win.Println(i,
		vaxis.Segment{Text: fmt.Sprintf("%-*s", 10, script.Name)},
		vaxis.Segment{Text: " ", Style: vaxis.Style{Foreground: vaxis.ColorBlack, Background: vaxis.IndexColor(uint8(script.Colour))}},
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
	running atomic.Bool
	lines   []string
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner, script *script) error {
	if !script.running.CompareAndSwap(false, true) {
		return nil
	}
	defer func() {
		script.running.Store(false)
	}()

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, script.Path)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
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

	vx.SyncFunc(func() {
		script.lines = lines
	})

	return nil
}

func runScriptItem(ctx context.Context, _ *vaxis.Vaxis, spinner *spinner, script *script, text string) (err error) {
	if !script.running.CompareAndSwap(false, true) {
		return nil
	}
	defer func() {
		script.running.Store(false)
	}()

	if spinner != nil {
		spinner.start()
		defer spinner.stop()
	}

	cmd := exec.CommandContext(ctx, script.Path, text)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	slog.InfoContext(ctx, "starting command", "args", cmd.Args)

	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func clamp[T cmp.Ordered](v, mn, mx T) T {
	v = max(v, mn)
	v = min(v, mx)
	return v
}

func match(str, s string) bool {
	str = strings.ToLower(str)
	s = strings.ToLower(s)
	return strings.Contains(str, s)
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

type config struct {
	Scripts []scriptConf `toml:"scripts"`
}

type scriptConf struct {
	Triggers []string `toml:"triggers"`
	Name     string   `toml:"name"`
	Path     string   `toml:"path"`
	Preview  int      `toml:"preview"`
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
