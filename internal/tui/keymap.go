package tui

import "strings"

// Binding is one key and what it does.
type Binding struct {
	// Keys are every key that triggers the binding; the first is the one help shows.
	Keys []string
	Help string
}

// Matches reports whether the pressed key triggers this binding.
func (b Binding) Matches(key string) bool {
	for _, k := range b.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Label is how the binding is written in help — the primary key, or a range for the sections.
func (b Binding) Label() string {
	switch len(b.Keys) {
	case 0:
		return ""
	case 1:
		return b.Keys[0]
	default:
		return strings.Join(b.Keys, "/")
	}
}

// KeyMap is the global keymap.
//
// It is the single source for both dispatch and the help overlay: Bindings drives `?`, and
// Update matches against these same values. A key that works but is undocumented, or is
// documented but dead, cannot exist without the two falling out of one struct.
type KeyMap struct {
	// Sections jumps to a section by number. Its Keys are generated from AllSections.
	Sections Binding
	Project  Binding
	Filter   Binding
	Help     Binding
	Quit     Binding
	// Cancel backs out of the help overlay or an in-progress filter.
	Cancel Binding
}

// DefaultKeyMap is the keymap ARCHITECTURE.md §9 specifies.
func DefaultKeyMap() KeyMap {
	keys := make([]string, 0, len(AllSections))
	for _, s := range AllSections {
		keys = append(keys, s.Key())
	}
	return KeyMap{
		Sections: Binding{Keys: keys, Help: "jump to a section"},
		Project:  Binding{Keys: []string{"p"}, Help: "cycle the project filter"},
		Filter:   Binding{Keys: []string{"/"}, Help: "filter the current screen"},
		Help:     Binding{Keys: []string{"?"}, Help: "toggle this help"},
		Quit:     Binding{Keys: []string{"q", "ctrl+c"}, Help: "quit"},
		Cancel:   Binding{Keys: []string{"esc"}, Help: "close help, or cancel a filter"},
	}
}

// Bindings lists every global binding in the order help presents them.
func (k KeyMap) Bindings() []Binding {
	return []Binding{k.Sections, k.Project, k.Filter, k.Help, k.Cancel, k.Quit}
}

// SectionFor returns the section a number key selects.
func (k KeyMap) SectionFor(key string) (Section, bool) {
	for _, s := range AllSections {
		if s.Key() == key {
			return s, true
		}
	}
	return 0, false
}
