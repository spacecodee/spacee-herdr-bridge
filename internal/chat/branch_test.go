package chat

import (
	"fmt"
	"strings"
	"testing"
)

// texts lists "role:first text" of every message, in order.
func texts(b *Builder) []string {
	var out []string
	for _, m := range snap(b) {
		s := m.Role + ":"
		if len(m.Parts) > 0 {
			s += m.Parts[0].Text
		}
		out = append(out, s)
	}
	return out
}

func same(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("messages = %q\nwant       %q", got, want)
	}
}

// --- Claude ---

func cu(uuid, parent, text string) string {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%s,"timestamp":"2026-11-03T10:00:00Z","message":{"role":"user","content":%q}}`, uuid, p, text)
}

func ca(uuid, parent, msgID, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":"2026-11-03T10:00:00Z","message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}]}}`, uuid, parent, msgID, text)
}

func TestClaudeRewindHidesTheAbandonedBranch(t *testing.T) {
	b := feed(claudeParser{},
		cu("u1", "", "first"),
		ca("a1", "u1", "m1", "answer one"),
		cu("u2", "a1", "old question"),
		ca("a2", "u2", "m2", "old answer"),
		`{"type":"attachment","uuid":"x1","parentUuid":"u2","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"old queued"}}`,
		`{"type":"system","uuid":"s1","parentUuid":"a2","level":"error","content":"old failure"}`,
		cu("u3", "a1", "new question"),
		ca("a3", "u3", "m3", "new answer"),
	)
	same(t, texts(b), "user:first", "assistant:answer one", "user:new question", "assistant:new answer")
}

func TestClaudeRewindWhileTailingRemovesTheAbandonedMessages(t *testing.T) {
	b := feed(claudeParser{},
		cu("u1", "", "first"),
		ca("a1", "u1", "m1", "answer one"),
		cu("u2", "a1", "old question"),
		ca("a2", "u2", "m2", "old answer"),
	)
	b.Drain()
	b.Gone()
	for _, l := range []string{cu("u3", "a1", "new question"), ca("a3", "u3", "m3", "new answer")} {
		claudeParser{}.Line([]byte(l), b)
	}
	same(t, texts(b), "user:first", "assistant:answer one", "user:new question", "assistant:new answer")
	added, _ := b.Drain()
	if len(added) != 2 {
		t.Fatalf("added = %+v", added)
	}
	if g := strings.Join(b.Gone(), ","); g != "u2,m2" {
		t.Fatalf("gone = %q, want u2,m2", g)
	}
}

func TestClaudeCompactionBoundaryKeepsEarlierMessages(t *testing.T) {
	b := feed(claudeParser{},
		cu("u1", "", "first"),
		ca("a1", "u1", "m1", "answer one"),
		`{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"logicalParentUuid":"a1","level":"info","content":"Conversation compacted"}`,
		cu("u2", "cb", "after compaction"),
		ca("a2", "u2", "m2", "answer two"),
	)
	same(t, texts(b), "user:first", "assistant:answer one", "user:after compaction", "assistant:answer two")
}

func TestClaudeTrailingSystemErrorAndSiblingToolResultsStay(t *testing.T) {
	b := feed(claudeParser{},
		cu("u1", "", "go"),
		ca("a1", "u1", "m1", "calling"),
		`{"type":"system","uuid":"s1","parentUuid":"a1","level":"error","content":"API error"}`,
	)
	same(t, texts(b), "user:go", "assistant:calling", "system:API error")
	// the next prompt hangs off the assistant record, not the error: the error stays
	claudeParser{}.Line([]byte(cu("u2", "a1", "again")), b)
	same(t, texts(b), "user:go", "assistant:calling", "system:API error", "user:again")
}

func TestClaudeNoRewindRendersAsBefore(t *testing.T) {
	b := feed(claudeParser{},
		`{"type":"queue-operation","operation":"enqueue","content":"later"}`,
		cu("u1", "", "first"),
		ca("a1", "u1", "m1", "one"),
		`{"type":"attachment","uuid":"x1","parentUuid":"a1","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"queued one"}}`,
		cu("u2", "x1", "second"),
		ca("a2", "u2", "m2", "two"),
	)
	same(t, texts(b), "user:later", "user:first", "assistant:one", "user:queued one", "user:second", "assistant:two")
	if b.Snapshot(10)[0].Queued != true {
		t.Error("queued message lost its flag")
	}
}

// --- pi ---

func pe(id, parent, role, text string) string {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-11-03T10:00:00Z","message":{"role":%q,"content":[{"type":"text","text":%q}]}}`, id, p, role, text)
}

func TestPiBranchFollowsTheLastEntry(t *testing.T) {
	b := feed(piParser{},
		`{"type":"session","id":"sess","cwd":"/x"}`,
		pe("p1", "", "user", "first"),
		pe("p2", "p1", "assistant", "one"),
		pe("p3", "p2", "user", "old"),
		pe("p4", "p3", "assistant", "old answer"),
		`{"type":"model_change","id":"mc","parentId":"p4"}`,
		pe("p5", "p2", "user", "new"),
		pe("p6", "p5", "assistant", "new answer"),
	)
	same(t, texts(b), "user:first", "assistant:one", "user:new", "assistant:new answer")
}

func TestPiBranchWhileTailing(t *testing.T) {
	b := feed(piParser{},
		pe("p1", "", "user", "first"),
		pe("p2", "p1", "assistant", "one"),
		pe("p3", "p2", "user", "old"),
	)
	b.Drain()
	b.Gone()
	piParser{}.Line([]byte(pe("p4", "p2", "user", "new")), b)
	same(t, texts(b), "user:first", "assistant:one", "user:new")
	if g := strings.Join(b.Gone(), ","); g != "p3" {
		t.Fatalf("gone = %q", g)
	}
}

func TestPiNoBranchRendersAsBefore(t *testing.T) {
	b := feed(piParser{},
		`{"type":"session","id":"sess"}`,
		pe("p1", "", "user", "a"),
		`{"type":"custom","id":"c","parentId":"p1"}`,
		pe("p2", "c", "assistant", "b"),
		pe("p3", "p2", "user", "c"),
	)
	same(t, texts(b), "user:a", "assistant:b", "user:c")
}

// --- Codex ---

func cx(role, id, text string) string {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return fmt.Sprintf(`{"timestamp":"2026-11-03T10:00:00Z","type":"response_item","payload":{"type":"message","id":%q,"role":%q,"content":[{"type":%q,"text":%q}]}}`, id, role, kind, text)
}

func rollback(n int) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-03T10:00:00Z","type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":%d}}`, n)
}

const cxEnv = `{"timestamp":"2026-11-03T10:00:00Z","type":"response_item","payload":{"type":"message","id":"env","role":"user","content":[{"type":"input_text","text":"<environment_context>\n</environment_context>"}]}}`

func cxCall(id, call string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-03T10:00:00Z","type":"response_item","payload":{"type":"function_call","id":%q,"call_id":%q,"name":"shell","arguments":"{}"}}`, id, call)
}

func TestCodexRollbackDropsTheLastTurns(t *testing.T) {
	b := feed(codexParser{},
		cxEnv,
		cx("user", "u1", "one"), cx("assistant", "a1", "ans one"),
		cx("user", "u2", "two"), cxCall("f2", "c2"), cx("assistant", "a2", "ans two"),
		cx("user", "u3", "three"), cx("assistant", "a3", "ans three"),
		rollback(2),
	)
	same(t, texts(b), "user:one", "assistant:ans one")
	// a new prompt after the rollback is a new turn
	codexParser{}.Line([]byte(cx("user", "u4", "four")), b)
	same(t, texts(b), "user:one", "assistant:ans one", "user:four")
}

func TestCodexRollbackWhileTailingReportsTheRemoved(t *testing.T) {
	b := feed(codexParser{}, cx("user", "u1", "one"), cx("assistant", "a1", "ans"), cx("user", "u2", "two"))
	b.Drain()
	b.Gone()
	codexParser{}.Line([]byte(rollback(1)), b)
	same(t, texts(b), "user:one", "assistant:ans")
	if g := strings.Join(b.Gone(), ","); g != "u2" {
		t.Fatalf("gone = %q", g)
	}
	// more turns than exist: everything goes, nothing panics
	codexParser{}.Line([]byte(rollback(9)), b)
	same(t, texts(b))
}

func TestCodexNoRollbackRendersAsBefore(t *testing.T) {
	b := feed(codexParser{}, cxEnv, cx("user", "u1", "one"), cx("assistant", "a1", "ans"),
		`{"timestamp":"2026-11-03T10:00:00Z","type":"event_msg","payload":{"type":"token_count"}}`)
	same(t, texts(b), "user:one", "assistant:ans")
}

// Parallel tool calls fork the tree: each result hangs off the call it answers. That is not
// a rewind, and nothing may disappear.
func TestClaudeParallelToolResultsAreNotARewind(t *testing.T) {
	tool := func(uuid, parent, msgID, call string) string {
		return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"message":{"id":%q,"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"bash","input":{"command":"ls"}}]}}`, uuid, parent, msgID, call)
	}
	result := func(uuid, parent, call string) string {
		return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, uuid, parent, call)
	}
	b := feed(claudeParser{},
		cu("u1", "", "go"),
		tool("a1", "u1", "m1", "t1"),
		tool("a2", "a1", "m2", "t2"),
		result("r1", "a1", "t1"),
		result("r2", "a2", "t2"),
		ca("a3", "r2", "m3", "done"),
		cu("u2", "a3", "next"),
	)
	got := snap(b)
	if len(got) != 5 {
		t.Fatalf("messages = %d: %q", len(got), texts(b))
	}
	if got[1].Parts[0].Status != "ok" || got[2].Parts[0].Status != "ok" {
		t.Errorf("tool results lost: %+v %+v", got[1], got[2])
	}
}
