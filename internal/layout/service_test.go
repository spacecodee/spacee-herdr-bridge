package layout

import (
	"errors"
	"reflect"
	"testing"
)

func newSvc(t *testing.T) (*Service, *fakeCore) {
	f := newFakeCore(t, fixtureJSON)
	return &Service{Core: &UnixCore{Socket: f.Socket}, Logf: func(string, ...any) {}}, f
}

func TestCreateSpaceStartsTerminalWithLabelsThenWritesDoc(t *testing.T) {
	s, f := newSvc(t)
	got, err := s.CreateSpace("Proj", "/home/me/proj", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Runs) != 1 {
		t.Fatalf("runs %d", len(f.Runs))
	}
	run := f.Runs[0]
	labels := run["labels"].(map[string]any)
	if labels["herdr.space"] != got.Space || labels["herdr.tab"] != got.Tab {
		t.Fatalf("labels %v vs %+v", labels, got)
	}
	if run["cwd"] != "/home/me/proj" {
		t.Fatalf("cwd %v", run["cwd"])
	}
	if _, has := run["argv"]; has {
		t.Fatalf("shell must not send argv: %v", run)
	}
	if _, has := run["on"]; has {
		t.Fatalf("local terminal must not send on: %v", run)
	}
	_, tab, sp := findTab(f.Doc(), got.Tab)
	if tab == nil || sp["id"] != got.Space || sp["name"] != "Proj" || sp["folder"] != "/home/me/proj" {
		t.Fatalf("doc lacks the space: %v", sp)
	}
	if terminalsUnder(asObj(tab["root"]))[0] != got.Terminal {
		t.Fatalf("pane does not hold %s", got.Terminal)
	}
}

func TestCreateSpaceRemoteAndAgentArgv(t *testing.T) {
	s, f := newSvc(t)
	if _, err := s.CreateSpace("R", "/srv", "rohan", []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	run := f.Runs[0]
	if run["on"] != "rohan" || !reflect.DeepEqual(run["argv"], []any{"claude"}) {
		t.Fatalf("run %v", run)
	}
}

func TestValidation(t *testing.T) {
	s, f := newSvc(t)
	cases := map[string]func() error{
		"argv not allowed":    func() error { _, e := s.CreateSpace("A", "/x", "", []string{"rm", "-rf"}); return e },
		"argv with args":      func() error { _, e := s.CreateSpace("A", "/x", "", []string{"claude", "--yolo"}); return e },
		"relative folder":     func() error { _, e := s.CreateSpace("A", "x/y", "", nil); return e },
		"empty folder":        func() error { _, e := s.CreateSpace("A", "", "", nil); return e },
		"empty name":          func() error { _, e := s.CreateSpace("  ", "/x", "", nil); return e },
		"long name":           func() error { _, e := s.CreateSpace(string(make([]byte, 200)), "/x", "", nil); return e },
		"unknown machine":     func() error { _, e := s.CreateSpace("A", "/x", "nope", nil); return e },
		"tab relative folder": func() error { _, e := s.CreateTab("spc1", "T", "rel", nil); return e },
		"tab bad argv":        func() error { _, e := s.CreateTab("spc1", "T", "", []string{"vim"}); return e },
		"split direction":     func() error { _, e := s.Split("tbB", "", "left", "", nil); return e },
		"rename empty":        func() error { return s.RenameSpace("spc1", "") },
	}
	for name, fn := range cases {
		if err := fn(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if len(f.Runs) != 0 || f.Sets != 0 {
		t.Fatalf("rejected input must touch nothing: runs %d sets %d", len(f.Runs), f.Sets)
	}
}

func TestConflictRetriesFromAFreshRead(t *testing.T) {
	s, f := newSvc(t)
	f.Conflicts = 2
	if _, err := s.CreateSpace("P", "/p", "", nil); err != nil {
		t.Fatal(err)
	}
	if f.Sets != 3 {
		t.Fatalf("sets %d", f.Sets)
	}
	if len(f.Runs) != 1 || len(f.Killed) != 0 {
		t.Fatalf("runs %d killed %v", len(f.Runs), f.Killed)
	}
}

func TestConflictGivesUpAfterThreeAndKillsTheTerminal(t *testing.T) {
	s, f := newSvc(t)
	f.Conflicts = 10
	_, err := s.CreateSpace("P", "/p", "", nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v", err)
	}
	if f.Sets != 3 {
		t.Fatalf("sets %d", f.Sets)
	}
	if !reflect.DeepEqual(f.Killed, []string{"t21"}) {
		t.Fatalf("orphan not killed: %v", f.Killed)
	}
}

func TestDocWriteFailureKillsNewTerminal(t *testing.T) {
	s, f := newSvc(t)
	f.FailSet = true
	if _, err := s.CreateTab("spc1", "T", "", nil); err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(f.Killed, []string{"t21"}) {
		t.Fatalf("killed %v", f.Killed)
	}
}

func TestTerminalFailureLeavesDocUntouched(t *testing.T) {
	s, f := newSvc(t)
	f.FailRun = true
	if _, err := s.CreateSpace("P", "/p", "", nil); err == nil {
		t.Fatal("want error")
	}
	if f.Sets != 0 {
		t.Fatalf("doc written %d times", f.Sets)
	}
}

func TestCreateTabUsesSpaceFolderAndLabels(t *testing.T) {
	s, f := newSvc(t)
	got, err := s.CreateTab("spc1", "Logs", "", []string{"pi"})
	if err != nil {
		t.Fatal(err)
	}
	run := f.Runs[0]
	if run["cwd"] != "/work" {
		t.Fatalf("cwd %v", run["cwd"])
	}
	labels := run["labels"].(map[string]any)
	if labels["herdr.space"] != "spc1" || labels["herdr.tab"] != got.Tab {
		t.Fatalf("labels %v", labels)
	}
	if _, err := s.CreateTab("nope", "x", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing space: %v", err)
	}
	if len(f.Runs) != 1 {
		t.Fatal("a missing space must not start a terminal")
	}
}

func TestSplitStartsTerminalInTheTab(t *testing.T) {
	s, f := newSvc(t)
	got, err := s.Split("tbB", "pB1", "right", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	labels := f.Runs[0]["labels"].(map[string]any)
	if labels["herdr.space"] != "spc1" || labels["herdr.tab"] != "tbB" {
		t.Fatalf("labels %v", labels)
	}
	_, pane, _ := findPane(f.Doc(), got.Pane)
	if pane == nil || terminalsUnder(pane)[0] != got.Terminal {
		t.Fatalf("pane %s missing in doc", got.Pane)
	}
	if _, err := s.Split("nope", "", "right", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tab: %v", err)
	}
}

func TestRenameAndMovePersist(t *testing.T) {
	s, f := newSvc(t)
	if err := s.RenameTab("tbA", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Move("space", "spc2", 0); err != nil {
		t.Fatal(err)
	}
	d := f.Doc()
	if d["spaces"].([]any)[0].(map[string]any)["id"] != "spc2" {
		t.Fatal("not moved")
	}
	if _, tab, _ := findTab(d, "tbA"); tab["name"] != "Renamed" {
		t.Fatal("not renamed")
	}
	if err := s.RenameSpace("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDeleteRemovesFromDocAndKillsTerminals(t *testing.T) {
	s, f := newSvc(t)
	if err := s.DeleteSpace("spc1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Killed, []string{"t1", "t2", "t3"}) {
		t.Fatalf("killed %v", f.Killed)
	}
	if i, sp := findSpace(f.Doc(), "spc1"); sp != nil || i != -1 {
		t.Fatal("space still in doc")
	}
	f.Killed = nil
	if err := s.DeletePane("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if len(f.Killed) != 0 {
		t.Fatal("nothing to kill for a missing pane")
	}
}

func TestDeleteTabAndPane(t *testing.T) {
	s, f := newSvc(t)
	if err := s.DeletePane("pB2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTab("tbB"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Killed, []string{"t3", "t2"}) {
		t.Fatalf("killed %v", f.Killed)
	}
	if err := s.DeleteTab("tbA"); !errors.Is(err, ErrConflict) {
		t.Fatalf("last tab: %v", err)
	}
}

func TestMissingDocStartsEmpty(t *testing.T) {
	f := newFakeCore(t, `null`)
	s := &Service{Core: &UnixCore{Socket: f.Socket}, Logf: func(string, ...any) {}}
	if _, err := s.CreateSpace("P", "/p", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(f.Doc()["spaces"].([]any)) != 1 {
		t.Fatal("space not written")
	}
}
