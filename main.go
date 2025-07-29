package main

import (
	"bufio"
	"cmp"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.sr.ht/~rockorager/vaxis"
	"git.sr.ht/~rockorager/vaxis/widgets/spinner"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func main() {
	// lf, _ := os.OpenFile("/tmp/cm", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	// _ = lf

	config, err := parseConfig("config.toml")
	if err != nil {
		panic(err)
	}

	scriptByName := make(map[string]scriptConf, len(config.Scripts))
	for _, s := range config.Scripts {
		scriptByName[s.Name] = s
	}

	vx, err := vaxis.New(vaxis.Options{})
	if err != nil {
		panic(err)
	}
	defer vx.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// elements
	spinner := spinner.New(vx, 50*time.Millisecond)
	spinner.Frames = []rune("▌▀▐▄")

	inp := textinput.
		New().
		SetPrompt("> ")

	var (
		data = map[string][]string{}
	)

	go func() {
		spinner.Start()
		defer spinner.Stop()

		var wg sync.WaitGroup
		for _, sc := range config.Scripts {
			if sc.Preview == 0 {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := loadScript(ctx, vx, data, sc); err != nil {
					panic(err)
				}
			}()
		}

		wg.Wait()
	}()

	// state
	type line struct{ group, text string }
	var (
		index     int
		visGroups []string
		visLines  []line
	)

	active := func() (int, scriptConf, string) {
		item := visLines[index]
		sconf := scriptByName[item.group]
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
					spinner.Start()
					defer spinner.Stop()

					if err := loadScript(ctx, vx, data, sconf); err != nil {
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

		visGroups = visGroups[:0]
		if left, rest, ok := strings.Cut(query, " "); ok {
			for _, t := range config.Triggers {
				if left == t.Key {
					visGroups = append(visGroups, t.Scripts...)
					query = rest
					break
				}
			}
		}
		if len(visGroups) == 0 {
			for _, sconf := range config.Scripts {
				visGroups = append(visGroups, sconf.Name)
			}
		}

		visLines = visLines[:0]
		for _, g := range visGroups {
			sconf := scriptByName[g]
			for i, item := range data[g] {
				if inpString == "" && i >= sconf.Preview {
					break
				}
				if strings.Contains(item, query) {
					visLines = append(visLines, line{group: g, text: item})
				}
			}
		}

		inpWin := win.New(0, 0, width, 1)
		inp.Draw(inpWin)

		listWin := win.New(0, 1, width, height-1)
		for i, it := range visLines {
			drawLine(listWin, i, scriptByName[it.group], it.text, i == index)
		}

		spinner.Draw(win)

		vx.Render()
	}
}

func drawLine(win vaxis.Window, i int, sconf scriptConf, text string, selected bool) {
	var style vaxis.Style
	if selected {
		style.Attribute = vaxis.AttrReverse
	}
	win.Println(i,
		vaxis.Segment{Text: sconf.Name, Style: vaxis.Style{Background: vaxis.IndexColor(uint8(sconf.Colour))}},
		vaxis.Segment{Text: " " + text, Style: style},
	)
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, data map[string][]string, sconf scriptConf) error {
	cmd := exec.CommandContext(ctx, sconf.Path)
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
		data[sconf.Name] = lines
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
	Scripts  []scriptConf
	Triggers []triggerConf
}

type scriptConf struct {
	Name    string
	Path    string
	Preview int
	Colour  int
}

type triggerConf struct {
	Key     string
	Scripts []string
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
