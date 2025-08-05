package main

import (
	"bufio"
	"cmp"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"git.sr.ht/~rockorager/vaxis"
	"git.sr.ht/~rockorager/vaxis/widgets/spinner"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func main() {
	lf, _ := os.OpenFile("/tmp/cm", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	_ = lf

	conf, err := parseConfig("config.toml")
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
	spinner := spinner.New(vx, 100*time.Millisecond)
	spinner.Frames = []rune("▌▀▐▄")

	inp := textinput.
		New().
		SetPrompt("> ")
	inp.Prompt = vaxis.Style{Foreground: vaxis.ColorBlack}

	spinner.Start()
	go func() {
		defer spinner.Stop()

		var wg sync.WaitGroup
		for _, sc := range scripts {
			if sc.Preview == 0 {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := loadScript(ctx, vx, nil, sc); err != nil {
					panic(err)
				}
			}()
		}

		wg.Wait()
	}()

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
				_, script, _ := active()
				go func() {
					if err := loadScript(ctx, vx, spinner, script); err != nil {
						panic(err)
					}
				}()
			case "Enter", "Shift+Enter":
				_, sconf, text := active()
				if err := runScriptItem(context.Background(), vx, sconf.Path, text); err != nil {
					panic(err)
				}
				// only quit if shift is not held
				if ev.Modifiers&vaxis.ModShift == 0 {
					return
				}
			}
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
				if strings.Contains(item, query) {
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
		spinner.Draw(spinWin)

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
	win.Println(i,
		vaxis.Segment{Text: script.Name, Style: vaxis.Style{Background: vaxis.IndexColor(uint8(script.Colour))}},
		vaxis.Segment{Text: " " + text, Style: style},
	)
}

func drawFooter(win vaxis.Window, conf config, visScripts []string) {
	footSegs := make([]vaxis.Segment, 0, len(conf.Scripts)*2)
	footSegs = append(footSegs, vaxis.Segment{Text: "# ", Style: vaxis.Style{Foreground: vaxis.ColorBlack}})

	for _, c := range conf.Scripts {
		if len(footSegs) > 1 {
			footSegs = append(footSegs, vaxis.Segment{Text: " "})
		}
		var style = vaxis.Style{Foreground: vaxis.ColorBlack}
		if slices.Contains(visScripts, c.Name) {
			style = vaxis.Style{}
		}
		footSegs = append(footSegs, vaxis.Segment{Text: c.Name, Style: style})
	}

	win.Println(0, footSegs...)
}

type script struct {
	scriptConf
	running atomic.Bool
	lines   []string
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, spinner *spinner.Model, script *script) error {
	if !script.running.CompareAndSwap(false, true) {
		return nil
	}
	defer func() {
		script.running.Store(false)
	}()

	if spinner != nil {
		spinner.Start()
		defer spinner.Stop()
	}

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

	sc := bufio.NewScanner(stdout)

	var lines []string
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

func runScriptItem(ctx context.Context, _ *vaxis.Vaxis, scriptPath string, text string) (err error) {
	cmd := exec.CommandContext(ctx, scriptPath, text)
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

type config struct {
	Scripts []scriptConf
}

type scriptConf struct {
	Triggers []string
	Name     string
	Path     string
	Preview  int
	Colour   int
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

func clamp[T cmp.Ordered](v, mn, mx T) T {
	v = max(v, mn)
	v = min(v, mx)
	return v
}
