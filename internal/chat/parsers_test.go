package chat

import (
	"strings"
	"testing"
)

func feed(p Parser, lines ...string) *Builder {
	b := NewBuilder(400)
	for _, l := range lines {
		p.Line([]byte(l), b)
	}
	return b
}

func snap(b *Builder) []Message { return b.Snapshot(1000) }

func partTypes(m Message) string {
	var t []string
	for _, p := range m.Parts {
		t = append(t, p.Type)
	}
	return strings.Join(t, ",")
}

// --- Claude Code ---

func TestClaudeTextThinkingToolAndResult(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"file-history-snapshot","messageId":"x","snapshot":{}}`,
		`{"type":"mode","mode":"auto","sessionId":"s"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-11-02T10:00:00Z","message":{"role":"user","content":"hello there"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-11-02T10:00:01Z","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"zz"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-11-02T10:00:02Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"on it"}]}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-11-02T10:00:03Z","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tc1","name":"Bash","input":{"command":"pnpm test","description":"run"}}]}}`,
	)
	got := snap(b)
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2 (user + merged assistant)", len(got))
	}
	if got[0].Role != "user" || got[0].ID != "u1" || got[0].Parts[0].Text != "hello there" || got[0].Time != "2026-11-02T10:00:00Z" {
		t.Errorf("user message = %+v", got[0])
	}
	if got[1].Role != "assistant" || got[1].ID != "m1" || partTypes(got[1]) != "thinking,text,tool" {
		t.Fatalf("assistant message = %+v", got[1])
	}
	tool := got[1].Parts[2]
	if tool.ID != "tc1" || tool.Name != "Bash" || tool.Summary != "pnpm test" || tool.Status != "running" {
		t.Errorf("tool = %+v", tool)
	}
	// result arrives
	b.Drain()
	claudeParser{}.Line([]byte(`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tc1","content":"all green","is_error":false}]}}`), b)
	added, upd := b.Drain()
	if len(added) != 0 || len(upd) != 1 || upd[0].ID != "m1" {
		t.Fatalf("added=%d updated=%+v, want one update of m1", len(added), upd)
	}
	if p := upd[0].Parts[2]; p.Status != "ok" || p.Output != "all green" {
		t.Errorf("tool after result = %+v", p)
	}
}

func TestClaudeToolErrorAndArrayContent(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"tool_use","id":"tc1","name":"Read","input":{"file_path":"/x/y.go"}}]}}`,
		`{"type":"user","uuid":"u","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tc1","is_error":true,"content":[{"type":"text","text":"no such file"}]}]}}`,
	)
	p := snap(b)[0].Parts[0]
	if p.Status != "error" || p.Output != "no such file" || p.Summary != "/x/y.go" {
		t.Errorf("tool = %+v", p)
	}
}

func TestClaudeImageNeverCarriesBase64(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAABBBB"}}]}}`,
	)
	m := snap(b)[0]
	if partTypes(m) != "text,image" || m.Parts[1].Note != "inline image" || m.Parts[1].Path != "" {
		t.Errorf("parts = %+v", m.Parts)
	}
	if strings.Contains(marshal(t, m), "AAAABBBB") {
		t.Error("base64 leaked into output")
	}
}

func TestClaudeSkipsInternalSidechainMetaAndGarbage(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"attachment","attachment":{}}`,
		`{"type":"api-request","id":"1"}`,
		`{"type":"user","uuid":"m","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"user","uuid":"s","isSidechain":true,"message":{"role":"user","content":"sub"}}`,
		`{"type":"assistant","uuid":"e","message":{"id":"e","content":[{"type":"text","text":"   "}]}}`,
		`{"type":"user","uuid":"p","message":{"role":"user","con`,
		`not json`,
		`{"type":"brand-new-record","x":1}`,
	)
	if n := len(snap(b)); n != 0 {
		t.Errorf("messages = %d, want 0", n)
	}
}

func TestClaudeSystemErrorKept(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"system","uuid":"s1","level":"info","content":"bookkeeping"}`,
		`{"type":"system","uuid":"s2","level":"error","timestamp":"2026-11-02T10:00:00Z","content":"API Error: overloaded"}`,
	)
	got := snap(b)
	if len(got) != 1 || got[0].Role != "system" || got[0].Parts[0].Text != "API Error: overloaded" {
		t.Errorf("got = %+v", got)
	}
}

func TestTruncation(t *testing.T) {
	big := strings.Repeat("é", 100<<10) // multi-byte: must cut on a rune boundary
	b := feed(claudeParser{},
		`{"type":"assistant","uuid":"a","message":{"id":"m","content":[{"type":"text","text":"`+big+`"},{"type":"tool_use","id":"t","name":"Bash","input":{"command":"`+strings.Repeat("x", 20<<10)+`"}}]}}`,
		`{"type":"user","uuid":"u","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"`+strings.Repeat("o", 20<<10)+`"}]}}`,
	)
	m := snap(b)[0]
	txt, tool := m.Parts[0], m.Parts[1]
	if !txt.Truncated || len(txt.Text) > 64<<10 || len(txt.Text) < 64<<10-4 {
		t.Errorf("text len=%d truncated=%v", len(txt.Text), txt.Truncated)
	}
	if !strings.HasSuffix(txt.Text, "é") {
		t.Error("text cut inside a rune")
	}
	if !tool.Truncated || len(tool.Input) != 8<<10 || len(tool.Output) != 8<<10 {
		t.Errorf("tool input=%d output=%d truncated=%v", len(tool.Input), len(tool.Output), tool.Truncated)
	}
	if tool.Summary != strings.Repeat("x", 80) {
		t.Errorf("summary = %d chars, want the command cut to 80", len(tool.Summary))
	}
}

// --- pi ---

func TestPiMessages(t *testing.T) {
	b := feed(piParser{},
		`{"type":"session","id":"s","cwd":"/x"}`,
		`{"type":"model_change","id":"mc"}`,
		`{"type":"custom","id":"c","customType":"x","data":{}}`,
		`{"type":"message","id":"p1","timestamp":"2026-11-02T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"do it"},{"type":"image","data":"QUJD","mimeType":"image/png"}]}}`,
		`{"type":"message","id":"p2","timestamp":"2026-11-02T10:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"ok"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"message","id":"p3","timestamp":"2026-11-02T10:00:02Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","isError":true,"content":[{"type":"text","text":"boom"}]}}`,
		`{"type":"message","id":"p4","message":{"role":"system","content":"prompt"}}`,
		`{"type":"message","id":"p5","timestamp":"2026-11-02T10:00:03Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"provider failed"}}`,
	)
	got := snap(b)
	if len(got) != 3 {
		t.Fatalf("messages = %d: %+v", len(got), got)
	}
	if got[0].Role != "user" || partTypes(got[0]) != "text,image" || got[0].Parts[1].Note != "inline image" {
		t.Errorf("user = %+v", got[0])
	}
	if partTypes(got[1]) != "thinking,text,tool" {
		t.Errorf("assistant parts = %s", partTypes(got[1]))
	}
	if p := got[1].Parts[2]; p.ID != "c1" || p.Summary != "ls" || p.Status != "error" || p.Output != "boom" {
		t.Errorf("tool = %+v", p)
	}
	if got[2].Role != "system" || got[2].Parts[0].Text != "provider failed" {
		t.Errorf("error message = %+v", got[2])
	}
}

// --- codex ---

func TestCodexMessages(t *testing.T) {
	b := feed(codexParser{},
		`{"timestamp":"2026-11-02T10:00:00Z","type":"session_meta","payload":{"cwd":"/x"}}`,
		`{"timestamp":"2026-11-02T10:00:00Z","type":"response_item","payload":{"type":"message","id":"d","role":"developer","content":[{"type":"input_text","text":"rules"}]}}`,
		`{"timestamp":"2026-11-02T10:00:01Z","type":"response_item","payload":{"type":"message","id":"e","role":"user","content":[{"type":"input_text","text":"<environment_context>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-11-02T10:00:02Z","type":"response_item","payload":{"type":"message","id":"u","role":"user","content":[{"type":"input_text","text":"fix it"},{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}}`,
		`{"timestamp":"2026-11-02T10:00:03Z","type":"response_item","payload":{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"thinking about it"}]}}`,
		`{"timestamp":"2026-11-02T10:00:04Z","type":"response_item","payload":{"type":"reasoning","id":"r0","summary":[]}}`,
		`{"timestamp":"2026-11-02T10:00:05Z","type":"response_item","payload":{"type":"function_call","id":"f","call_id":"c1","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"go test\"]}"}}`,
		`{"timestamp":"2026-11-02T10:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"{\"output\":\"ok\",\"metadata\":{\"exit_code\":0}}"}}`,
		`{"timestamp":"2026-11-02T10:00:07Z","type":"response_item","payload":{"type":"custom_tool_call","id":"g","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"}}`,
		`{"timestamp":"2026-11-02T10:00:08Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c2","output":[{"type":"input_text","text":"Exit code: 1\nfailed"}]}}`,
		`{"timestamp":"2026-11-02T10:00:09Z","type":"response_item","payload":{"type":"message","id":"a","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"timestamp":"2026-11-02T10:00:10Z","type":"event_msg","payload":{"type":"token_count"}}`,
	)
	got := snap(b)
	var roles []string
	for _, m := range got {
		roles = append(roles, m.Role+":"+partTypes(m))
	}
	want := "user:text,image assistant:thinking assistant:tool assistant:tool assistant:text"
	if strings.Join(roles, " ") != want {
		t.Fatalf("messages = %q, want %q", strings.Join(roles, " "), want)
	}
	if p := got[2].Parts[0]; p.Name != "shell" || p.Status != "ok" || p.Output != "ok" || p.Summary != "bash -lc go test" {
		t.Errorf("function_call tool = %+v", p)
	}
	if p := got[3].Parts[0]; p.Name != "apply_patch" || p.Status != "error" || !strings.Contains(p.Output, "failed") {
		t.Errorf("custom tool = %+v", p)
	}
	if got[0].Parts[1].Note != "inline image" || strings.Contains(marshal(t, got[0]), "QUJD") {
		t.Errorf("image = %+v", got[0].Parts[1])
	}
}

func TestClaudeQueuedMessageShowsUntilDelivered(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"user","uuid":"u1","timestamp":"2026-11-03T10:00:00Z","message":{"role":"user","content":"start"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-03T10:00:05Z","sessionId":"s","content":"also check the docs"}`,
	)
	got := snap(b)
	if len(got) != 2 {
		t.Fatalf("messages = %d, want 2 (user + queued)", len(got))
	}
	q := got[1]
	if q.Role != "user" || !q.Queued || q.Parts[0].Text != "also check the docs" || q.Time != "2026-11-03T10:00:05Z" {
		t.Fatalf("queued message = %+v", q)
	}
	b.Drain()
	b.Gone()

	// Delivered mid-turn: the queued entry goes away and the prompt is a normal user message.
	claudeParser{}.Line([]byte(`{"type":"attachment","uuid":"att1","timestamp":"2026-11-03T10:00:09Z","attachment":{"type":"queued_command","prompt":"also check the docs","commandMode":"prompt","origin":{"kind":"human"}}}`), b)
	claudeParser{}.Line([]byte(`{"type":"queue-operation","operation":"remove","timestamp":"2026-11-03T10:00:09Z","sessionId":"s","content":"also check the docs"}`), b)
	added, _ := b.Drain()
	if len(added) != 1 || added[0].ID != "att1" || added[0].Role != "user" || added[0].Queued || added[0].Parts[0].Text != "also check the docs" {
		t.Fatalf("added = %+v, want the delivered prompt", added)
	}
	if gone := b.Gone(); len(gone) != 1 || gone[0] != q.ID {
		t.Fatalf("gone = %v, want [%s]", gone, q.ID)
	}
	if n := len(snap(b)); n != 2 {
		t.Errorf("messages after delivery = %d, want 2", n)
	}
}

func TestClaudeDequeuePopsTheOldestQueued(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"queue-operation","operation":"enqueue","timestamp":"t1","content":"first"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"t2","content":"second"}`,
		`{"type":"queue-operation","operation":"dequeue","timestamp":"t3"}`,
	)
	got := snap(b)
	if len(got) != 1 || got[0].Parts[0].Text != "second" || !got[0].Queued {
		t.Fatalf("left = %+v, want only the queued second", got)
	}
}

func TestClaudeTaskNotificationsAreNotShownAsQueued(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"queue-operation","operation":"enqueue","timestamp":"t1","content":"<task-notification>\n<task-id>x</task-id>"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"t2","content":"mine"}`,
		`{"type":"attachment","uuid":"att","attachment":{"type":"queued_command","prompt":"<task-notification>x","commandMode":"task-notification"}}`,
		// The notification leaves the queue first; "mine" must stay.
		`{"type":"queue-operation","operation":"dequeue","timestamp":"t3"}`,
	)
	got := snap(b)
	if len(got) != 1 || got[0].Parts[0].Text != "mine" || !got[0].Queued {
		t.Fatalf("messages = %+v, want only the queued \"mine\"", got)
	}
}

func TestBuilderRemovingAnUnseenMessageIsNotReported(t *testing.T) {
	b := NewBuilder(10)
	b.Enqueue("x", Message{Role: "user", Parts: []Part{{Type: "text", Text: "x"}}})
	b.Dequeue("")
	added, upd := b.Drain()
	if len(added)+len(upd) != 0 {
		t.Errorf("added=%+v updated=%+v, want nothing", added, upd)
	}
	if gone := b.Gone(); len(gone) != 0 {
		t.Errorf("gone = %v, want none (the client never saw it)", gone)
	}
}

// --- builder ---

func TestBuilderAddedThenUpdatedInOneBatchIsAdded(t *testing.T) {
	b := NewBuilder(10)
	b.Add(Message{ID: "m", Role: "assistant", Parts: []Part{{Type: "tool", ID: "t", Name: "Bash", Status: "running"}}})
	b.Result("t", "x", false)
	added, upd := b.Drain()
	if len(added) != 1 || len(upd) != 0 || added[0].Parts[0].Output != "x" {
		t.Errorf("added=%+v updated=%+v", added, upd)
	}
	if a, u := b.Drain(); len(a)+len(u) != 0 {
		t.Error("second drain not empty")
	}
}

func TestBuilderKeepsOnlyTheLastN(t *testing.T) {
	b := NewBuilder(3)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		b.Add(Message{ID: id, Role: "user", Parts: []Part{{Type: "text", Text: id}}})
	}
	got := snap(b)
	if len(got) != 3 || got[0].ID != "3" {
		t.Errorf("kept = %+v", got)
	}
	if last := b.Snapshot(2); len(last) != 2 || last[0].ID != "4" {
		t.Errorf("snapshot(2) = %+v", last)
	}
	b.Result("nope", "x", false) // unknown call id: ignored
}

func TestClaudeSlashCommandMarkup(t *testing.T) {
	cmd := func(name, args string) string {
		return "<command-name>" + name + "</command-name>\\n            <command-message>" + name[1:] + "</command-message>\\n            <command-args>" + args + "</command-args>"
	}
	line := func(id, content string) string {
		return `{"type":"user","uuid":"` + id + `","timestamp":"T","message":{"role":"user","content":"` + content + `"}}`
	}
	b := feed(claudeParser{},
		line("c1", cmd("/clear", "")),
		line("c2", cmd("/model", "opus")),
		line("o1", "<local-command-stdout>Set model to opus</local-command-stdout>"),
		line("o2", "<local-command-stdout></local-command-stdout>"),
		line("p1", "use <command-name>x</command-name> in prose"),
	)
	ms := snap(b)
	if len(ms) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(ms), ms)
	}
	want := []struct{ id, role, text string }{
		{"c1", "user", "/clear"},
		{"c2", "user", "/model opus"},
		{"o1", "system", "Set model to opus"},
		{"p1", "user", "use <command-name>x</command-name> in prose"},
	}
	for i, w := range want {
		m := ms[i]
		if m.ID != w.id || m.Role != w.role || m.Time != "T" || len(m.Parts) != 1 || m.Parts[0].Text != w.text {
			t.Errorf("message %d = %+v, want %+v", i, m, w)
		}
	}
}
