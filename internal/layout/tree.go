// Package layout edits the herdr-core document "herdr-app" (spaces, tabs, panes) and
// starts or kills the terminals those panes hold, so the layout can be managed from the
// web client with the desktop window closed.
//
// The edits are pure functions over the decoded document. Unknown fields are kept as
// they are: the window owns the format and may write fields this package never reads.
package layout

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// Doc is the decoded value of the herdr-app document.
type Doc = map[string]any

type obj = map[string]any

// decodeDoc keeps numbers as json.Number so untouched values are written back verbatim.
func decodeDoc(raw []byte) (Doc, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return Doc{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var d Doc
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if d == nil {
		d = Doc{}
	}
	return d, nil
}

// newID returns prefix + 32 random hex characters, like the window's ids.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + hex.EncodeToString(b[:])
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := strconv.ParseFloat(n.String(), 64)
		return f, err == nil
	}
	return 0, false
}

func spaces(d Doc) []any {
	s, _ := d["spaces"].([]any)
	return s
}

func asObj(v any) obj { o, _ := v.(map[string]any); return o }

func tabsOf(space obj) []any { t, _ := space["tabs"].([]any); return t }

func children(n obj) []any { c, _ := n["children"].([]any); return c }

func findSpace(d Doc, id string) (int, obj) {
	for i, s := range spaces(d) {
		if o := asObj(s); o["id"] == id {
			return i, o
		}
	}
	return -1, nil
}

// findTab returns the tab's index inside its space, the tab and its space.
func findTab(d Doc, id string) (int, obj, obj) {
	for _, s := range spaces(d) {
		sp := asObj(s)
		for i, t := range tabsOf(sp) {
			if tab := asObj(t); tab["id"] == id {
				return i, tab, sp
			}
		}
	}
	return -1, nil, nil
}

func block(id, terminal string) obj {
	return obj{"type": "block", "id": id, "content": obj{"terminal": terminal}}
}

// newTab builds a tab like the window: a row whose id is the tab id, holding one block.
func newTab(tabID, name, paneID, terminal string) obj {
	return obj{"id": tabID, "name": name, "root": obj{
		"type": "row", "id": tabID, "children": []any{block(paneID, terminal)},
	}}
}

// AddSpace appends a space with one tab ("Shell") holding one pane.
func AddSpace(d Doc, spaceID, name, folder, tabID, paneID, terminal string) {
	d["spaces"] = append(spaces(d), obj{
		"id": spaceID, "name": name, "folder": folder,
		"tabs": []any{newTab(tabID, "Shell", paneID, terminal)},
	})
}

// AddTab appends a tab with one pane to a space.
func AddTab(d Doc, spaceID, tabID, name, paneID, terminal string) error {
	_, sp := findSpace(d, spaceID)
	if sp == nil {
		return fmt.Errorf("space %w", ErrNotFound)
	}
	sp["tabs"] = append(tabsOf(sp), newTab(tabID, name, paneID, terminal))
	return nil
}

// location of a node inside its parent's children.
type location struct {
	parent obj
	index  int
}

// walk visits every node under n, giving each its parent (nil for n itself).
func walk(n, parent obj, visit func(node, parent obj, index int) bool) bool {
	for i, c := range children(n) {
		ch := asObj(c)
		if visit(ch, n, i) {
			return true
		}
		if walk(ch, n, visit) {
			return true
		}
	}
	return false
}

func findNode(root obj, id string) (obj, location, bool) {
	var found obj
	var loc location
	ok := walk(root, nil, func(n, p obj, i int) bool {
		if n["id"] == id {
			found, loc = n, location{p, i}
			return true
		}
		return false
	})
	return found, loc, ok
}

// findPane looks a pane (block) up in every tab.
func findPane(d Doc, id string) (tab, pane obj, loc location) {
	for _, s := range spaces(d) {
		for _, t := range tabsOf(asObj(s)) {
			tb := asObj(t)
			if n, l, ok := findNode(asObj(tb["root"]), id); ok && n["type"] == "block" {
				return tb, n, l
			}
		}
	}
	return nil, nil, location{}
}

func terminalsUnder(n obj) []string {
	var out []string
	if n["type"] == "block" {
		if c := asObj(n["content"]); c != nil {
			if t, ok := c["terminal"].(string); ok {
				out = append(out, t)
			}
		}
		return out
	}
	walk(n, nil, func(c, _ obj, _ int) bool {
		if c["type"] == "block" {
			if t, ok := asObj(c["content"])["terminal"].(string); ok {
				out = append(out, t)
			}
		}
		return false
	})
	return out
}

func weightOf(n obj) (float64, bool) {
	s := asObj(n["size"])
	if s == nil {
		return 0, false
	}
	return toFloat(s["weight"])
}

func setWeight(n obj, w float64) { n["size"] = obj{"weight": w} }

// Split puts a new pane (holding terminal) next to the pane paneID (the tab's last pane
// when empty), like the window: inside the parent when it already runs in that
// direction, otherwise by wrapping the pane in a new container. It returns the id of
// the pane it split.
func Split(d Doc, tabID, paneID, direction, newPaneID, containerID, terminal string) (string, error) {
	kind := map[string]string{"right": "row", "down": "column"}[direction]
	if kind == "" {
		return "", fmt.Errorf("direction %w", ErrInvalid)
	}
	_, tab, _ := findTab(d, tabID)
	if tab == nil {
		return "", fmt.Errorf("tab %w", ErrNotFound)
	}
	root := asObj(tab["root"])
	var anchor obj
	var loc location
	if paneID == "" {
		walk(root, nil, func(n, p obj, i int) bool {
			if n["type"] == "block" {
				anchor, loc = n, location{p, i}
			}
			return false
		})
	} else if n, l, ok := findNode(root, paneID); ok && n["type"] == "block" {
		anchor, loc = n, l
	}
	if anchor == nil {
		return "", fmt.Errorf("pane %w", ErrNotFound)
	}
	fresh := block(newPaneID, terminal)
	parent := loc.parent
	if parent["type"] == kind {
		sibs := children(parent)
		all := true
		for _, s := range sibs {
			if _, ok := weightOf(asObj(s)); !ok {
				all = false
			}
		}
		out := make([]any, 0, len(sibs)+1)
		out = append(out, sibs[:loc.index+1]...)
		out = append(out, fresh)
		out = append(out, sibs[loc.index+1:]...)
		parent["children"] = out
		if w, ok := weightOf(anchor); ok && all {
			setWeight(anchor, w/2)
			setWeight(fresh, w/2)
		} else {
			for _, s := range out {
				setWeight(asObj(s), 1/float64(len(out)))
			}
		}
		return fmt.Sprint(anchor["id"]), nil
	}
	box := obj{"type": kind, "id": containerID}
	if w, ok := weightOf(anchor); ok {
		box["size"] = obj{"weight": w}
	}
	setWeight(anchor, 0.5)
	setWeight(fresh, 0.5)
	box["children"] = []any{anchor, fresh}
	parent["children"].([]any)[loc.index] = box
	return fmt.Sprint(anchor["id"]), nil
}

// renormalize scales sibling weights to sum 1 when they all have one.
func renormalize(parent obj) {
	var sum float64
	sibs := children(parent)
	for _, s := range sibs {
		w, ok := weightOf(asObj(s))
		if !ok || w <= 0 {
			return
		}
		sum += w
	}
	for _, s := range sibs {
		w, _ := weightOf(asObj(s))
		setWeight(asObj(s), w/sum)
	}
}

// RemovePane drops a pane and returns its terminal. A tab's last pane stays (delete the
// tab instead); a container left with one child is replaced by that child.
func RemovePane(d Doc, paneID string) ([]string, error) {
	tab, pane, loc := findPane(d, paneID)
	if pane == nil {
		return nil, fmt.Errorf("pane %w", ErrNotFound)
	}
	if len(terminalsUnder(asObj(tab["root"]))) < 2 {
		return nil, fmt.Errorf("last pane of the tab, delete the tab: %w", ErrConflict)
	}
	parent := loc.parent
	sibs := children(parent)
	parent["children"] = append(append([]any{}, sibs[:loc.index]...), sibs[loc.index+1:]...)
	remaining := children(parent)
	switch {
	case len(remaining) == 0:
		// The parent was a nested container holding only this pane: drop it too.
		_, gl, _ := findContainer(asObj(tab["root"]), parent)
		gp := gl.parent
		gs := children(gp)
		gp["children"] = append(append([]any{}, gs[:gl.index]...), gs[gl.index+1:]...)
		renormalize(gp)
	case len(remaining) == 1 && parent["id"] != tab["id"]:
		only := asObj(remaining[0])
		if w, ok := weightOf(parent); ok {
			setWeight(only, w)
		} else {
			delete(only, "size")
		}
		_, gl, _ := findContainer(asObj(tab["root"]), parent)
		gl.parent["children"].([]any)[gl.index] = only
	default:
		renormalize(parent)
	}
	return terminalsUnder(pane), nil
}

func findContainer(root, target obj) (obj, location, bool) {
	var loc location
	ok := walk(root, nil, func(n, p obj, i int) bool {
		if sameObj(n, target) {
			loc = location{p, i}
			return true
		}
		return false
	})
	return target, loc, ok
}

// sameObj compares map identity through their ids (ids are unique in a tab).
func sameObj(a, b obj) bool { return a["id"] != nil && a["id"] == b["id"] }

// RemoveTab drops a tab and returns its terminals. A space keeps at least one tab.
func RemoveTab(d Doc, tabID string) ([]string, error) {
	i, tab, sp := findTab(d, tabID)
	if tab == nil {
		return nil, fmt.Errorf("tab %w", ErrNotFound)
	}
	tabs := tabsOf(sp)
	if len(tabs) < 2 {
		return nil, fmt.Errorf("last tab of the space, delete the space: %w", ErrConflict)
	}
	sp["tabs"] = append(append([]any{}, tabs[:i]...), tabs[i+1:]...)
	return terminalsUnder(asObj(tab["root"])), nil
}

// RemoveSpace drops a space and returns every terminal in it.
func RemoveSpace(d Doc, spaceID string) ([]string, error) {
	i, sp := findSpace(d, spaceID)
	if sp == nil {
		return nil, fmt.Errorf("space %w", ErrNotFound)
	}
	var terms []string
	for _, t := range tabsOf(sp) {
		terms = append(terms, terminalsUnder(asObj(asObj(t)["root"]))...)
	}
	all := spaces(d)
	d["spaces"] = append(append([]any{}, all[:i]...), all[i+1:]...)
	return terms, nil
}

func RenameSpace(d Doc, id, name string) error {
	_, sp := findSpace(d, id)
	if sp == nil {
		return fmt.Errorf("space %w", ErrNotFound)
	}
	sp["name"] = name
	return nil
}

func RenameTab(d Doc, id, name string) error {
	_, tab, _ := findTab(d, id)
	if tab == nil {
		return fmt.Errorf("tab %w", ErrNotFound)
	}
	tab["name"] = name
	return nil
}

func moveTo(list []any, from, to int) []any {
	item := list[from]
	rest := append(append([]any{}, list[:from]...), list[from+1:]...)
	out := append([]any{}, rest[:to]...)
	out = append(out, item)
	return append(out, rest[to:]...)
}

// Move puts a space (among spaces) or a tab (inside its space) at index.
func Move(d Doc, kind, id string, index int) error {
	switch kind {
	case "space":
		i, sp := findSpace(d, id)
		if sp == nil {
			return fmt.Errorf("space %w", ErrNotFound)
		}
		if index < 0 || index >= len(spaces(d)) {
			return fmt.Errorf("index %w", ErrInvalid)
		}
		d["spaces"] = moveTo(spaces(d), i, index)
	case "tab":
		i, tab, sp := findTab(d, id)
		if tab == nil {
			return fmt.Errorf("tab %w", ErrNotFound)
		}
		if index < 0 || index >= len(tabsOf(sp)) {
			return fmt.Errorf("index %w", ErrInvalid)
		}
		sp["tabs"] = moveTo(tabsOf(sp), i, index)
	default:
		return fmt.Errorf("kind %w", ErrInvalid)
	}
	return nil
}
