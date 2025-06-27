package listw

import (
	"slices"

	"git.sr.ht/~rockorager/vaxis"
)

type Item interface {
	Draw(selected bool) []vaxis.Segment
}

type List[T Item] struct {
	index    int
	items    []T
	filtered []T
}

func New[T Item]() *List[T] {
	return &List[T]{}
}

func (m *List[T]) Draw(win vaxis.Window) {
	for i, item := range m.filtered {
		win.Println(i, item.Draw(i == m.index)...)
	}
}

func (m *List[T]) Append(items []T) {
	m.items = append(m.items, items...)
}

func (m *List[T]) FilterFunc(f func(T) bool) {
	m.filtered = nil

	for _, s := range m.items {
		if f(s) {
			m.filtered = append(m.filtered, s)
		}
	}

	if total := m.totalVisibleItems(); m.index >= total {
		m.End()
	}
}

func (m *List[T]) SortFunc(cmp func(a, b T) int) {
	slices.SortStableFunc(m.filtered, cmp)
}

func (m *List[T]) totalVisibleItems() int {
	return len(m.filtered)
}

func (m *List[T]) ActiveItem() (T, bool) {
	if m.index >= 0 && m.index < len(m.filtered) {
		return m.filtered[m.index], true
	}
	var zero T
	return zero, false
}

func (m *List[T]) Down() {
	m.index = min(m.totalVisibleItems()-1, m.index+1)
}

func (m *List[T]) Up() {
	m.index = max(0, m.index-1)
}

func (m *List[T]) Home() {
	m.index = 0
}

func (m *List[T]) End() {
	m.index = m.totalVisibleItems() - 1
}

func (m *List[T]) PageDown(win vaxis.Window) {
	_, height := win.Size()
	m.index = min(m.totalVisibleItems()-1, m.index+height)
}

func (m *List[T]) PageUp(win vaxis.Window) {
	_, height := win.Size()
	m.index = max(0, m.index-height)
}
