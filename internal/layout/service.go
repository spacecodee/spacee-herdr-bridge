package layout

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"strings"
	"time"
	"unicode"
)

const (
	docName      = "herdr-app"
	maxAttempts  = 3
	callTimeout  = 5 * time.Second
	maxNameRunes = 80
)

// allowedArgv is the closed list of programs the web client may start: a shell
// (no argv) or one of the agents, without arguments.
var allowedArgv = map[string]bool{"claude": true, "pi": true, "codex": true, "opencode": true, "agy": true}

// Core is the slice of herdr-core's JSON-RPC API this package uses.
type Core interface {
	Call(method string, params, out any) error
}

// RPCError is an error answered by core; Kind is its data.kind ("conflict", ...).
type RPCError struct{ Kind, Message string }

func (e *RPCError) Error() string { return "core: " + e.Kind }

// UnixCore calls core over its Unix socket, one connection per call.
type UnixCore struct{ Socket string }

func (c *UnixCore) Call(method string, params, out any) error {
	conn, err := net.DialTimeout("unix", c.Socket, callTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(callTimeout))
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return err
	}
	line, err := bufio.NewReaderSize(conn, 1<<20).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Data    struct {
				Kind string `json:"kind"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		kind := resp.Error.Data.Kind
		if kind == "" {
			kind = resp.Error.Message
		}
		return &RPCError{Kind: kind, Message: resp.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Service runs the layout operations.
type Service struct {
	Core Core
	Logf func(format string, args ...any) // operation names and outcomes only
}

// Created names what a create call made.
type Created struct {
	Space    string `json:"space,omitempty"`
	Tab      string `json:"tab,omitempty"`
	Pane     string `json:"pane"`
	Terminal string `json:"terminal"`
}

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func invalid(what string) error { return fmt.Errorf("%s: %w", what, ErrInvalid) }

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxNameRunes {
		return "", invalid("name")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", invalid("name")
		}
	}
	return name, nil
}

func checkFolder(folder string) error {
	if !path.IsAbs(folder) || len(folder) > 4096 || strings.ContainsRune(folder, 0) {
		return invalid("folder")
	}
	return nil
}

func checkArgv(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	if len(argv) != 1 || !allowedArgv[argv[0]] {
		return invalid("argv")
	}
	return nil
}

func (s *Service) checkMachine(machine string) error {
	if machine == "" {
		return nil
	}
	var r struct {
		Machines []struct {
			Name string `json:"name"`
		} `json:"machines"`
	}
	if err := s.Core.Call("machine.list", map[string]any{}, &r); err != nil {
		return err
	}
	for _, m := range r.Machines {
		if m.Name == machine {
			return nil
		}
	}
	return invalid("machine")
}

// read returns the document and its version (an unwritten document is empty).
func (s *Service) read() (Doc, uint64, error) {
	var r struct {
		Value   json.RawMessage `json:"value"`
		Version uint64          `json:"version"`
	}
	if err := s.Core.Call("doc.get", map[string]any{"name": docName}, &r); err != nil {
		return nil, 0, err
	}
	d, err := decodeDoc(r.Value)
	return d, r.Version, err
}

// mutate reads the document, applies fn and writes it back with the version it read.
// A version conflict retries from a fresh read, up to three attempts.
func (s *Service) mutate(fn func(Doc) error) error {
	for i := 0; i < maxAttempts; i++ {
		d, version, err := s.read()
		if err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
		err = s.Core.Call("doc.set", map[string]any{"name": docName, "value": d, "version": version}, nil)
		var rpc *RPCError
		if errors.As(err, &rpc) && rpc.Kind == "conflict" {
			continue
		}
		return err
	}
	return fmt.Errorf("layout changed while writing: %w", ErrConflict)
}

// start runs a terminal that opts into the window through its labels.
func (s *Service) start(space, tab, cwd, machine string, argv []string) (string, error) {
	p := map[string]any{"labels": map[string]string{"herdr.space": space, "herdr.tab": tab}}
	if len(argv) > 0 {
		p["argv"] = argv
	}
	if cwd != "" {
		p["cwd"] = cwd
	}
	if machine != "" {
		p["on"] = machine
	}
	var t struct {
		Address string `json:"address"`
	}
	if err := s.Core.Call("terminal.run", p, &t); err != nil {
		return "", err
	}
	if t.Address == "" {
		return "", errors.New("core: terminal without address")
	}
	return t.Address, nil
}

func (s *Service) kill(terminals ...string) {
	for _, t := range terminals {
		if err := s.Core.Call("terminal.kill", map[string]any{"terminal": t}, nil); err != nil {
			var rpc *RPCError
			if !errors.As(err, &rpc) || rpc.Kind != "not_found" {
				s.logf("layout kill failed")
			}
		}
	}
}

// create starts the terminal first, then writes the doc; a failed write kills it.
func (s *Service) create(op, space, tab, cwd, machine string, argv []string, edit func(Doc, string) error) (string, error) {
	term, err := s.start(space, tab, cwd, machine, argv)
	if err != nil {
		s.logf("layout %s failed", op)
		return "", err
	}
	if err := s.mutate(func(d Doc) error { return edit(d, term) }); err != nil {
		s.kill(term)
		s.logf("layout %s failed", op)
		return "", err
	}
	s.logf("layout %s ok", op)
	return term, nil
}

// CreateSpace adds a space with one tab and starts its first terminal in folder.
func (s *Service) CreateSpace(name, folder, machine string, argv []string) (Created, error) {
	name, err := cleanName(name)
	if err != nil {
		return Created{}, err
	}
	if err := checkFolder(folder); err != nil {
		return Created{}, err
	}
	if err := checkArgv(argv); err != nil {
		return Created{}, err
	}
	if err := s.checkMachine(machine); err != nil {
		return Created{}, err
	}
	c := Created{Space: newID("spc"), Tab: newID("tb"), Pane: newID("p")}
	c.Terminal, err = s.create("space.create", c.Space, c.Tab, folder, machine, argv, func(d Doc, term string) error {
		AddSpace(d, c.Space, name, folder, c.Tab, c.Pane, term)
		return nil
	})
	return c, err
}

// CreateTab adds a tab with a new terminal; folder defaults to the space's folder.
func (s *Service) CreateTab(space, name, folder string, argv []string) (Created, error) {
	name, err := cleanName(name)
	if err != nil {
		return Created{}, err
	}
	if folder != "" {
		if err := checkFolder(folder); err != nil {
			return Created{}, err
		}
	}
	if err := checkArgv(argv); err != nil {
		return Created{}, err
	}
	d, _, err := s.read()
	if err != nil {
		return Created{}, err
	}
	_, sp := findSpace(d, space)
	if sp == nil {
		return Created{}, fmt.Errorf("space %w", ErrNotFound)
	}
	if folder == "" {
		folder, _ = sp["folder"].(string)
	}
	c := Created{Space: space, Tab: newID("tb"), Pane: newID("p")}
	c.Terminal, err = s.create("tab.create", space, c.Tab, folder, "", argv, func(d Doc, term string) error {
		return AddTab(d, space, c.Tab, name, c.Pane, term)
	})
	return c, err
}

// Split adds a terminal next to a pane (the tab's last one when pane is empty).
func (s *Service) Split(tab, pane, direction string, folder string, argv []string) (Created, error) {
	if direction != "right" && direction != "down" {
		return Created{}, invalid("direction")
	}
	if folder != "" {
		if err := checkFolder(folder); err != nil {
			return Created{}, err
		}
	}
	if err := checkArgv(argv); err != nil {
		return Created{}, err
	}
	d, _, err := s.read()
	if err != nil {
		return Created{}, err
	}
	_, tb, sp := findTab(d, tab)
	if tb == nil {
		return Created{}, fmt.Errorf("tab %w", ErrNotFound)
	}
	if folder == "" {
		folder, _ = sp["folder"].(string)
	}
	spaceID, _ := sp["id"].(string)
	c := Created{Space: spaceID, Tab: tab, Pane: newID("p")}
	container := newID("c")
	c.Terminal, err = s.create("pane.split", spaceID, tab, folder, "", argv, func(d Doc, term string) error {
		_, e := Split(d, tab, pane, direction, c.Pane, container, term)
		return e
	})
	return c, err
}

func (s *Service) edit(op string, fn func(Doc) error) error {
	err := s.mutate(fn)
	if err != nil {
		s.logf("layout %s failed", op)
		return err
	}
	s.logf("layout %s ok", op)
	return nil
}

func (s *Service) RenameSpace(id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.edit("space.rename", func(d Doc) error { return RenameSpace(d, id, name) })
}

func (s *Service) RenameTab(id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.edit("tab.rename", func(d Doc) error { return RenameTab(d, id, name) })
}

func (s *Service) Move(kind, id string, index int) error {
	return s.edit(kind+".move", func(d Doc) error { return Move(d, kind, id, index) })
}

// remove drops the node from the doc, then ends its terminals.
func (s *Service) remove(op string, fn func(Doc) ([]string, error)) error {
	var terms []string
	err := s.edit(op, func(d Doc) (err error) { terms, err = fn(d); return })
	if err != nil {
		return err
	}
	s.kill(terms...)
	return nil
}

func (s *Service) DeleteSpace(id string) error {
	return s.remove("space.delete", func(d Doc) ([]string, error) { return RemoveSpace(d, id) })
}

func (s *Service) DeleteTab(id string) error {
	return s.remove("tab.delete", func(d Doc) ([]string, error) { return RemoveTab(d, id) })
}

func (s *Service) DeletePane(id string) error {
	return s.remove("pane.delete", func(d Doc) ([]string, error) { return RemovePane(d, id) })
}
