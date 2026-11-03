package layout

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// fixture mirrors the real herdr-app doc: tab root is a row whose id equals the tab id,
// leaves are blocks, sibling blocks carry size.weight.
const fixtureJSON = `{"spaces":[
 {"id":"spc1","name":"One","folder":"/work","tabs":[
  {"id":"tbA","name":"A","root":{"type":"row","id":"tbA","children":[
    {"type":"block","id":"pA1","content":{"terminal":"t1"}}]}},
  {"id":"tbB","name":"B","root":{"type":"row","id":"tbB","children":[
    {"type":"block","id":"pB1","size":{"weight":0.5},"content":{"terminal":"t2"}},
    {"type":"block","id":"pB2","size":{"weight":0.5},"content":{"terminal":"t3"}}]}}]},
 {"id":"spc2","name":"Two","folder":"","tabs":[
  {"id":"tbC","name":"C","root":{"type":"row","id":"tbC","children":[
    {"type":"block","id":"pC1","content":{"terminal":"t4"}}]}}]}]}`

func fixture(t *testing.T) Doc {
	t.Helper()
	d, err := decodeDoc([]byte(fixtureJSON))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func tabRoot(t *testing.T, d Doc, id string) map[string]any {
	t.Helper()
	_, tab, _ := findTab(d, id)
	if tab == nil {
		t.Fatalf("tab %s missing", id)
	}
	return tab["root"].(map[string]any)
}

// wt reads a node's size.weight whether it came from JSON (json.Number) or from code.
func wt(n any) float64 {
	w := n.(map[string]any)["size"].(map[string]any)["weight"]
	f, _ := toFloat(w)
	return f
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAddSpaceMirrorsWindowShape(t *testing.T) {
	d := fixture(t)
	AddSpace(d, "spcN", "New", "/tmp", "tbN", "pN", "t9")
	sp := d["spaces"].([]any)
	if len(sp) != 3 {
		t.Fatalf("spaces %d", len(sp))
	}
	want := `{"folder":"/tmp","id":"spcN","name":"New","tabs":[{"id":"tbN","name":"Shell","root":{"children":[{"content":{"terminal":"t9"},"id":"pN","type":"block"}],"id":"tbN","type":"row"}}]}`
	if got := jsonOf(sp[2]); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestAddTab(t *testing.T) {
	d := fixture(t)
	if err := AddTab(d, "spc2", "tbN", "Tab N", "pN", "t9"); err != nil {
		t.Fatal(err)
	}
	_, _, sp := findTab(d, "tbN")
	if sp["id"] != "spc2" || len(sp["tabs"].([]any)) != 2 {
		t.Fatalf("tab not appended to spc2")
	}
	if err := AddTab(d, "nope", "x", "n", "p", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestSplitRightInRowSplitsWeight(t *testing.T) {
	d := fixture(t)
	if _, err := Split(d, "tbB", "pB1", "right", "pN", "cN", "t9"); err != nil {
		t.Fatal(err)
	}
	ch := tabRoot(t, d, "tbB")["children"].([]any)
	if len(ch) != 3 {
		t.Fatalf("children %d", len(ch))
	}
	ids := []string{ch[0].(map[string]any)["id"].(string), ch[1].(map[string]any)["id"].(string), ch[2].(map[string]any)["id"].(string)}
	if !reflect.DeepEqual(ids, []string{"pB1", "pN", "pB2"}) {
		t.Fatalf("order %v", ids)
	}
	if w := wt(ch[0].(map[string]any)); w != 0.25 {
		t.Fatalf("anchor weight %v", w)
	}
	if w := wt(ch[1].(map[string]any)); w != 0.25 {
		t.Fatalf("new weight %v", w)
	}
	if w := wt(ch[2].(map[string]any)); w != 0.5 {
		t.Fatalf("other weight %v", w)
	}
}

func TestSplitFromSingleBlockEqualizes(t *testing.T) {
	d := fixture(t)
	anchor, err := Split(d, "tbA", "", "right", "pN", "cN", "t9")
	if err != nil || anchor != "pA1" {
		t.Fatalf("anchor %q err %v", anchor, err)
	}
	want := `[{"content":{"terminal":"t1"},"id":"pA1","size":{"weight":0.5},"type":"block"},{"content":{"terminal":"t9"},"id":"pN","size":{"weight":0.5},"type":"block"}]`
	if got := jsonOf(tabRoot(t, d, "tbA")["children"]); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestSplitDownWrapsPaneInColumn(t *testing.T) {
	d := fixture(t)
	if _, err := Split(d, "tbB", "pB2", "down", "pN", "cN", "t9"); err != nil {
		t.Fatal(err)
	}
	ch := tabRoot(t, d, "tbB")["children"].([]any)
	col := ch[1].(map[string]any)
	if col["type"] != "column" || col["id"] != "cN" {
		t.Fatalf("not wrapped: %s", jsonOf(col))
	}
	if wt(col) != 0.5 {
		t.Fatalf("container keeps the pane's old size: %s", jsonOf(col))
	}
	want := `[{"content":{"terminal":"t3"},"id":"pB2","size":{"weight":0.5},"type":"block"},{"content":{"terminal":"t9"},"id":"pN","size":{"weight":0.5},"type":"block"}]`
	if got := jsonOf(col["children"]); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestSplitErrors(t *testing.T) {
	d := fixture(t)
	if _, err := Split(d, "zzz", "", "right", "p", "c", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tab: %v", err)
	}
	if _, err := Split(d, "tbB", "zzz", "right", "p", "c", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pane: %v", err)
	}
	if _, err := Split(d, "tbB", "", "left", "p", "c", "t"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("direction: %v", err)
	}
}

func TestRemovePane(t *testing.T) {
	d := fixture(t)
	terms, err := RemovePane(d, "pB1")
	if err != nil || !reflect.DeepEqual(terms, []string{"t2"}) {
		t.Fatalf("terms %v err %v", terms, err)
	}
	ch := tabRoot(t, d, "tbB")["children"].([]any)
	if len(ch) != 1 || wt(ch[0].(map[string]any)) != 1.0 {
		t.Fatalf("weights not renormalized: %s", jsonOf(ch))
	}
	if _, err := RemovePane(d, "pB2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("last pane: %v", err)
	}
	if _, err := RemovePane(d, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestRemovePaneCollapsesSingleChildContainer(t *testing.T) {
	d := fixture(t)
	Split(d, "tbB", "pB2", "down", "pN", "cN", "t9")
	if _, err := RemovePane(d, "pN"); err != nil {
		t.Fatal(err)
	}
	ch := tabRoot(t, d, "tbB")["children"].([]any)
	last := ch[1].(map[string]any)
	if last["type"] != "block" || last["id"] != "pB2" || wt(last) != 0.5 {
		t.Fatalf("container not collapsed: %s", jsonOf(ch))
	}
}

func TestRemoveTab(t *testing.T) {
	d := fixture(t)
	terms, err := RemoveTab(d, "tbB")
	if err != nil || !reflect.DeepEqual(terms, []string{"t2", "t3"}) {
		t.Fatalf("terms %v err %v", terms, err)
	}
	if _, tab, _ := findTab(d, "tbB"); tab != nil {
		t.Fatal("tab still there")
	}
	if _, err := RemoveTab(d, "tbA"); !errors.Is(err, ErrConflict) {
		t.Fatalf("last tab of a space: %v", err)
	}
	if _, err := RemoveTab(d, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestRemoveSpace(t *testing.T) {
	d := fixture(t)
	terms, err := RemoveSpace(d, "spc1")
	if err != nil || !reflect.DeepEqual(terms, []string{"t1", "t2", "t3"}) {
		t.Fatalf("terms %v err %v", terms, err)
	}
	if len(d["spaces"].([]any)) != 1 {
		t.Fatal("space still there")
	}
	if _, err := RemoveSpace(d, "spc1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestRename(t *testing.T) {
	d := fixture(t)
	if err := RenameSpace(d, "spc2", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if d["spaces"].([]any)[1].(map[string]any)["name"] != "Renamed" {
		t.Fatal("space not renamed")
	}
	if err := RenameTab(d, "tbC", "Tab"); err != nil {
		t.Fatal(err)
	}
	if _, tab, _ := findTab(d, "tbC"); tab["name"] != "Tab" {
		t.Fatal("tab not renamed")
	}
	if err := RenameTab(d, "x", "n"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestMove(t *testing.T) {
	d := fixture(t)
	if err := Move(d, "space", "spc2", 0); err != nil {
		t.Fatal(err)
	}
	if d["spaces"].([]any)[0].(map[string]any)["id"] != "spc2" {
		t.Fatal("space not moved")
	}
	if err := Move(d, "tab", "tbB", 0); err != nil {
		t.Fatal(err)
	}
	tabs := d["spaces"].([]any)[1].(map[string]any)["tabs"].([]any)
	if tabs[0].(map[string]any)["id"] != "tbB" {
		t.Fatalf("tab not moved: %s", jsonOf(tabs))
	}
	if err := Move(d, "tab", "tbB", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("index: %v", err)
	}
	if err := Move(d, "pane", "x", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("kind: %v", err)
	}
	if err := Move(d, "space", "nope", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDecodeKeepsUnknownFieldsAndNumbers(t *testing.T) {
	in := `{"spaces":[],"extra":{"n":12345678901234567890,"f":1.5}}`
	d, err := decodeDoc([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonOf(d); got != `{"extra":{"f":1.5,"n":12345678901234567890},"spaces":[]}` {
		t.Fatalf("got %s", got)
	}
}

func TestNewIDShape(t *testing.T) {
	id := newID("spc")
	if len(id) != 3+32 || id[:3] != "spc" || newID("spc") == id {
		t.Fatalf("bad id %q", id)
	}
}
