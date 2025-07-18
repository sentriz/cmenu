package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"git.sr.ht/~rockorager/vaxis"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func main() {
	lf, _ := os.OpenFile("/tmp/cm", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	_ = lf

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

	// spinner := spinner.New(vx, 50*time.Millisecond)
	// spinner.Frames = []rune("▀▐▄▌")

	data := map[string][]string{}

	var (
		activeIndex int
	)

	for _, sc := range config.Scripts {
		if sc.Preview > 0 {
			go func() {
				if err := loadScript(ctx, vx, data, sc); err != nil {
					panic(err)
				}
			}()
		}
	}

	inp := textinput.
		New().
		SetPrompt("> ")

	for ev := range vx.Events() {
		win := vx.Window()
		win.Clear()

		width, height := win.Size()

		switch ev := ev.(type) {
		case vaxis.Key:
			switch ev.String() {
			case "Ctrl+c", "q":
				return
			case "Down", "j":
				// activeIndex++?
				_ = activeIndex
			case "Up", "k":
			case "End":
			case "Home":
			case "Page_Down":
			case "Page_Up":
			case "Right":
				// some active item
				// go func() {
				// 	if err := loadScript(ctx, vx, list, scriptByName[item.script]); err != nil {
				// 		panic(err)
				// 	}
				// }()
			case "Enter":
				// some active item
				// if err := runScriptItem(ctx, vx, scriptByName[item.script].Path, item); err != nil {
				// 	panic(err)
				// }
				return
			}
		case vaxis.SyncFunc:
			ev()
		}

		inp.Update(ev)

		query := inp.String()

		var activeScripts []scriptConf
		if left, rest, ok := strings.Cut(query, " "); ok {
			for _, t := range config.Triggers {
				if left == t.Key {
					for _, scriptName := range t.Scripts {
						activeScripts = append(activeScripts, scriptByName[scriptName])
					}
					query = rest
					break
				}
			}
		}

		if len(activeScripts) == 0 {
			activeScripts = config.Scripts
		}

		inpWin := win.New(0, 0, width, 1)
		inp.Draw(inpWin)

		listWin := win.New(0, 1, width, height-1)

		var i int
		for _, sconf := range activeScripts {
			for _, item := range data[sconf.Name] {
				if query != "" && !strings.Contains(item, query) {
					continue
				}
				drawLine(listWin, i, sconf, item, false)
				i++
			}
		}

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
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: output: %q", err, string(output))
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
