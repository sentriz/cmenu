package main

import (
	"bufio"
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
	config, err := parseConfig("config.toml")
	if err != nil {
		panic(err)
	}

	confForScript := func(name string) scriptConf {
		for _, s := range config.Scripts {
			if s.Name == name {
				return s
			}
		}
		return scriptConf{}
	}

	_ = confForScript

	vx, err := vaxis.New(vaxis.Options{})
	if err != nil {
		panic(err)
	}
	defer vx.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var groups []*Group
	for _, sc := range config.Scripts {
		spinner := spinner.New(vx, 50*time.Millisecond)
		spinner.Frames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
		group := &Group{
			heading: sc.Name,
			spinner: spinner,
		}

		if sc.Preview > 0 {
			group.expanded = true

			go func() {
				if err := loadScript(ctx, vx, &sync.RWMutex{}, group, sc.Path); err != nil {
					panic(err)
				}
			}()
		}

		groups = append(groups, group)
	}

	list := NewList(groups)

	inp := textinput.
		New().
		SetPrompt("> ")

	var vl sync.RWMutex

	for ev := range vx.Events() {
		vl.RLock()

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
			case "Left":
				if hdx, g := list.ActiveGroup(); g != nil {
					list.GroupCollapse(hdx, g)
				}

			case "Right":
				if _, g := list.ActiveGroup(); g != nil {
					list.GroupExpand(g)
					go func() {
						if err := loadScript(ctx, vx, &vl, g, confForScript(g.heading).Path); err != nil {
							panic(err)
						}
					}()
				}
			}
		case vaxis.SyncFunc:
			ev()
		}

		inp.Update(ev)

		list.Filter(inp.String())

		inpWin := win.New(0, 0, width, 1)
		inp.Draw(inpWin)

		listWin := win.New(0, 1, width, height-1)
		list.Draw(listWin)

		vx.Render()

		vl.RUnlock()
	}
}

type Group struct {
	spinner *spinner.Model

	heading  string
	items    []string
	filtered []string
	expanded bool
}

type List struct {
	index  int
	groups []*Group
}

func NewList(groups []*Group) List {
	return List{groups: groups}
}

func (m *List) Draw(win vaxis.Window) {
	defaultStyle := vaxis.Style{}
	selectedStyle := vaxis.Style{Attribute: vaxis.AttrReverse}

	var i int
	for _, g := range m.groups {
		style := defaultStyle
		if i == m.index {
			style = selectedStyle
		}

		win.Println(i, vaxis.Segment{Text: g.heading, Style: style})
		g.spinner.Draw(win.New(len(g.heading)+1, i, 1, 1))

		i++

		if g.expanded {
			for _, item := range g.filtered {
				style = defaultStyle
				if i == m.index {
					style = selectedStyle
				}
				win.Println(i, vaxis.Segment{Text: "  " + item, Style: style})
				i++
			}
		}
	}
}

func (m *List) Filter(query string) {
	if query == "" {
		for _, g := range m.groups {
			g.filtered = g.items
		}
		return
	}

	for _, g := range m.groups {
		g.filtered = nil
		for _, s := range g.items {
			if strings.Contains(strings.ToLower(s), query) {
				g.filtered = append(g.filtered, s)
			}
		}
	}

	if total := m.totalVisibleItems(); m.index >= total {
		m.End()
	}
}

func (m *List) GroupExpand(g *Group) {
	g.expanded = true
}
func (m *List) GroupCollapse(headerIdx int, g *Group) {
	g.expanded = false
	if m.index > headerIdx {
		m.index = headerIdx
	}
}

func (m *List) ActiveGroup() (int, *Group) {
	var idx int
	for _, g := range m.groups {
		if m.index == idx {
			return idx, g
		}
		idx++

		if g.expanded {
			if m.index < idx+len(g.filtered) {
				return idx - 1, g
			}
			idx += len(g.filtered)
		}
	}
	return -1, nil
}

func (m *List) totalVisibleItems() int {
	var total int
	for _, g := range m.groups {
		total++ // heading
		if g.expanded {
			total += len(g.filtered)
		}
	}
	return total
}

func (m *List) Down() {
	m.index = min(m.totalVisibleItems()-1, m.index+1)
}

func (m *List) Up() {
	m.index = max(0, m.index-1)
}

func (m *List) Home() {
	m.index = 0
}

func (m *List) End() {
	m.index = m.totalVisibleItems() - 1
}

func (m *List) PageDown(win vaxis.Window) {
	_, height := win.Size()
	m.index = min(m.totalVisibleItems()-1, m.index+height)
}

func (m *List) PageUp(win vaxis.Window) {
	_, height := win.Size()
	m.index = max(0, m.index-height)
}

func loadScript(ctx context.Context, vx *vaxis.Vaxis, vl *sync.RWMutex, g *Group, script string) error {
	g.spinner.Start()

	defer func() {
		time.Sleep(20 * time.Millisecond) // make sure we can see it
		g.spinner.Stop()
	}()

	defer vx.PostEvent(vaxis.Redraw{})

	cmd := exec.CommandContext(ctx, script)
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

	time.Sleep(600 * time.Millisecond)

	vl.Lock()

	g.items = nil

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		g.items = append(g.items, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}

	vl.Unlock()

	if err := cmd.Wait(); err != nil {
		return err
	}
	return nil
}

type config struct {
	Scripts []scriptConf
}
type scriptConf struct {
	Name    string
	Path    string
	Preview int
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
