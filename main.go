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
	"slices"
	"strings"
	"syscall"

	"scrap/listw"

	"git.sr.ht/~rockorager/vaxis"
	"git.sr.ht/~rockorager/vaxis/widgets/textinput"
	"github.com/BurntSushi/toml"
)

func main() {
	var handler = slog.DiscardHandler
	if true {
		logFile, err := os.OpenFile("/tmp/cml", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
		if err != nil {
			panic(err)
		}
		defer func() {
			_ = logFile.Close()
		}()

		handler = slog.NewTextHandler(logFile, &slog.HandlerOptions{})
	}

	slog.SetDefault(slog.New(handler))

	config, err := parseConfig("config.toml")
	if err != nil {
		panic(err)
	}

	slog.Info("starting cmenu")

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

	list := listw.New[ScriptLine]()

	for _, sc := range config.Scripts {
		if sc.Preview > 0 {
			go func() {
				if err := loadScript(ctx, vx, list, sc); err != nil {
					panic(err)
				}
			}()
		}
	}

	inp := textinput.
		New().
		SetPrompt("> ")

	for ev := range vx.Events() {
		slog.Info("new ev", "ev", fmt.Sprintf("%T", ev))

		win := vx.Window()
		win.Clear()

		width, height := win.Size()

		switch ev := ev.(type) {
		case vaxis.Key:
			switch ev.String() {
			case "Ctrl+c", "q":
				return
			case "Down", "j":
				list.Down()
			case "Up", "k":
				list.Up()
			case "End":
				list.End()
			case "Home":
				list.Home()
			case "Page_Down":
				list.PageDown(win)
			case "Page_Up":
				list.PageUp(win)
			case "Enter":
				item, ok := list.ActiveItem()
				if !ok {
					continue
				}
				if err := runScriptItem(ctx, vx, scriptByName[item.script].Path, item); err != nil {
					panic(err)
				}
				return
			}
		case vaxis.SyncFunc:
			ev()
		}

		inp.Update(ev)

		query := inp.String()

		var filterScripts []string
		if left, rest, ok := strings.Cut(query, " "); ok {
			for _, t := range config.Triggers {
				if left == t.Key {
					filterScripts = t.Scripts
					query = rest
					break
				}
			}
		}

		list.FilterFunc(func(s ScriptLine) bool {
			if len(filterScripts) == 0 {
				return true
			}
			return slices.Contains(filterScripts, s.script)
		})

		list.SortFunc(func(a, b ScriptLine) int {
			return cmp.Compare(slices.Index(filterScripts, a.script), slices.Index(filterScripts, b.script))
		})

		inpWin := win.New(0, 0, width, 1)
		inp.Draw(inpWin)

		listWin := win.New(0, 1, width, height-1)
		list.Draw(listWin)

		vx.Render()
	}
}

type ScriptLine struct {
	colour int
	script string
	text   string
}

func (i ScriptLine) FilterText() string { return i.text }
func (i ScriptLine) Draw(selected bool) []vaxis.Segment {
	var style vaxis.Style
	if selected {
		style.Attribute = vaxis.AttrReverse
	}
	return []vaxis.Segment{
		{Text: i.script, Style: vaxis.Style{Background: vaxis.IndexColor(uint8(i.colour))}},
		{Text: " " + i.text, Style: style},
	}
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, m *listw.List[ScriptLine], sconf scriptConf) error {
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

	var lines []ScriptLine
	for sc.Scan() {
		lines = append(lines, ScriptLine{
			script: sconf.Name,
			text:   sc.Text(),
			colour: sconf.Colour,
		})
	}

	if err := sc.Err(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return err
	}

	vx.SyncFunc(func() {
		m.Append(lines)
	})

	return nil
}

func runScriptItem(ctx context.Context, _ *vaxis.Vaxis, scriptPath string, item ScriptLine) (err error) {
	cmd := exec.CommandContext(ctx, scriptPath, item.text)
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
