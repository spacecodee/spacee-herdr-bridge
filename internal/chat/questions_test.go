package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const askUse = `{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which db?","header":"DB","multiSelect":true,"options":[{"label":"pg","description":"Postgres"},{"label":"sqlite","description":"Embedded"}]}]}}]}}`

const askResult = `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions"}]},"toolUseResult":{"questions":[],"answers":{"Which db?":"pg"},"annotations":{}}}`

func TestClaudeAskUserQuestionPending(t *testing.T) {
	b := feed(claudeParser{}, askUse)
	p := snap(b)[0].Parts[0]
	if p.Type != "tool" || p.Status != "running" || len(p.Questions) != 1 {
		t.Fatalf("part = %+v", p)
	}
	q := p.Questions[0]
	if q.Question != "Which db?" || q.Header != "DB" || !q.MultiSelect || len(q.Options) != 2 ||
		q.Options[1].Label != "sqlite" || q.Options[1].Description != "Embedded" {
		t.Fatalf("question = %+v", q)
	}
	if p.Answers != nil {
		t.Fatalf("answers = %v", p.Answers)
	}
}

func TestClaudeAskUserQuestionAnswered(t *testing.T) {
	b := feed(claudeParser{}, askUse)
	b.Drain()
	claudeParser{}.Line([]byte(askResult), b)
	_, upd := b.Drain()
	if len(upd) != 1 {
		t.Fatalf("updated = %d", len(upd))
	}
	p := upd[0].Parts[0]
	if p.Status != "ok" || p.Answers["Which db?"] != "pg" || len(p.Questions) != 1 {
		t.Fatalf("part = %+v", p)
	}
	if p.Output != "User has answered your questions" {
		t.Fatalf("output = %q", p.Output)
	}
}

func TestClaudeAskUserQuestionAnsweredWithoutToolUseResult(t *testing.T) {
	b := feed(claudeParser{}, askUse,
		`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"done"}]}}`)
	p := snap(b)[0].Parts[0]
	if p.Status != "ok" || p.Answers != nil || len(p.Questions) != 1 {
		t.Fatalf("part = %+v", p)
	}
}

func TestClaudeAskUserQuestionMalformedFallsBack(t *testing.T) {
	for _, in := range []string{`"nope"`, `{"questions":"x"}`, `{"questions":[]}`, `{"questions":[{"question":"","options":[]}]}`, `{}`} {
		line := fmt.Sprintf(`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":%s}]}}`, in)
		b := feed(claudeParser{}, line)
		ms := snap(b)
		if len(ms) != 1 || len(ms[0].Parts) != 1 {
			t.Fatalf("%s: messages = %+v", in, ms)
		}
		p := ms[0].Parts[0]
		if p.Type != "tool" || p.Name != "AskUserQuestion" || p.Status != "running" || p.Questions != nil {
			t.Fatalf("%s: part = %+v", in, p)
		}
	}
	// A malformed toolUseResult keeps the questions and leaves answers empty.
	b := feed(claudeParser{}, askUse,
		`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"x"}]},"toolUseResult":"oops"}`)
	if p := snap(b)[0].Parts[0]; p.Status != "ok" || p.Answers != nil {
		t.Fatalf("part = %+v", p)
	}
}

func TestClaudeAskUserQuestionBounds(t *testing.T) {
	var qs, opts []string
	for i := 0; i < 6; i++ {
		opts = append(opts, fmt.Sprintf(`{"label":%q,"description":"d"}`, strings.Repeat("x", maxQText+10)))
	}
	opts = append(opts, `{"label":"seventh"}`)
	for i := 0; i < 5; i++ {
		qs = append(qs, fmt.Sprintf(`{"question":%q,"header":"h","options":[%s]}`, strings.Repeat("q", maxQText+10), strings.Join(opts, ",")))
	}
	line := fmt.Sprintf(`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[%s]}}]}}`, strings.Join(qs, ","))
	p := snap(feed(claudeParser{}, line))[0].Parts[0]
	if len(p.Questions) != 4 || len(p.Questions[0].Options) != 6 {
		t.Fatalf("questions=%d options=%d", len(p.Questions), len(p.Questions[0].Options))
	}
	if len(p.Questions[0].Question) != maxQText || len(p.Questions[0].Options[0].Label) != maxQText || !p.Truncated {
		t.Fatalf("not clipped: %d %d truncated=%v", len(p.Questions[0].Question), len(p.Questions[0].Options[0].Label), p.Truncated)
	}
}

func TestClaudeAskUserQuestionAnswersBounded(t *testing.T) {
	big := strings.Repeat("a", maxQText+5)
	var kv []string
	for i := 0; i < 10; i++ {
		kv = append(kv, fmt.Sprintf(`"k%d":%q`, i, big))
	}
	res := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"x"}]},"toolUseResult":{"answers":{%s}}}`, strings.Join(kv, ","))
	p := snap(feed(claudeParser{}, askUse, res))[0].Parts[0]
	if len(p.Answers) != maxQuestions {
		t.Fatalf("answers = %d", len(p.Answers))
	}
	for _, v := range p.Answers {
		if len(v) != maxQText {
			t.Fatalf("answer len %d", len(v))
		}
	}
}

func TestAskUserQuestionResetShowsPending(t *testing.T) {
	b := feed(claudeParser{}, askUse)
	b.Drain()
	snapshot := b.Snapshot(10) // what a reconnect "reset" sends
	raw, _ := json.Marshal(snapshot)
	if !strings.Contains(string(raw), `"questions":[{"question":"Which db?"`) || strings.Contains(string(raw), `"answers"`) {
		t.Fatalf("snapshot = %s", raw)
	}
}

// --- pi (ask_user_question / ask_user_choice extension tools) ---

const piAsk = `{"type":"message","id":"p1","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"ask_user_question","arguments":{"questions":[{"header":"DB","question":"Which db?","multiSelect":true,"options":[{"label":"pg","description":"Postgres"},{"label":"sqlite","description":"Embedded"}]},{"header":"Mode","question":"Which mode?","multiSelect":false,"options":[{"label":"fast","description":"f"}]}]}}]}}`

func TestPiAskUserQuestionPendingAndAnswered(t *testing.T) {
	b := feed(piParser{}, piAsk)
	p := snap(b)[0].Parts[0]
	if p.Status != "running" || len(p.Questions) != 2 || !p.Questions[0].MultiSelect || p.Questions[0].Options[1].Label != "sqlite" {
		t.Fatalf("pending = %+v", p)
	}
	b.Drain()
	piParser{}.Line([]byte(`{"type":"message","id":"p2","message":{"role":"toolResult","toolCallId":"c1","toolName":"ask_user_question","isError":false,"content":[{"type":"text","text":"answered"}],"details":{"answers":[{"questionIndex":0,"question":"Which db?","kind":"options","selected":["pg","sqlite"]},{"questionIndex":1,"question":"Which mode?","kind":"other","answer":"turbo"}]}}}`), b)
	_, upd := b.Drain()
	if len(upd) != 1 {
		t.Fatalf("updated = %d", len(upd))
	}
	p = upd[0].Parts[0]
	if p.Status != "ok" || p.Answers["Which db?"] != "pg, sqlite" || p.Answers["Which mode?"] != "turbo" {
		t.Fatalf("answered = %+v", p)
	}
}

func TestPiAskUserQuestionCancelledHasNoAnswers(t *testing.T) {
	b := feed(piParser{}, piAsk,
		`{"type":"message","id":"p2","message":{"role":"toolResult","toolCallId":"c1","toolName":"ask_user_question","isError":false,"content":[{"type":"text","text":"cancelled"}],"details":{"answers":[{"questionIndex":0,"question":"Which db?","kind":"options","answer":null}]}}}`)
	if p := snap(b)[0].Parts[0]; p.Status != "ok" || len(p.Answers) != 0 {
		t.Fatalf("part = %+v", p)
	}
}

func TestPiAskUserChoice(t *testing.T) {
	b := feed(piParser{},
		`{"type":"message","id":"p1","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"ask_user_choice","arguments":{"question":"Ship it?","options":[{"label":"Yes","description":"go","value":"y"},{"label":"No","description":"stop","value":"n"}]}}]}}`)
	p := snap(b)[0].Parts[0]
	if len(p.Questions) != 1 || p.Questions[0].Question != "Ship it?" || p.Questions[0].MultiSelect || len(p.Questions[0].Options) != 2 {
		t.Fatalf("pending = %+v", p)
	}
	piParser{}.Line([]byte(`{"type":"message","id":"p2","message":{"role":"toolResult","toolCallId":"c1","toolName":"ask_user_choice","content":[{"type":"text","text":"Yes"}],"details":{"question":"Ship it?","options":[],"selection":{"value":"y","label":"Yes","index":0}}}}`), b)
	if p = snap(b)[0].Parts[0]; p.Answers["Ship it?"] != "Yes" || p.Status != "ok" {
		t.Fatalf("answered = %+v", p)
	}
}

func TestPiAskUserQuestionMalformedFallsBack(t *testing.T) {
	b := feed(piParser{}, `{"type":"message","id":"p1","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"ask_user_question","arguments":{"questions":"x"}}]}}`)
	if p := snap(b)[0].Parts[0]; p.Type != "tool" || p.Questions != nil {
		t.Fatalf("part = %+v", p)
	}
}

// --- Codex (request_user_input, Plan mode) ---

const codexAsk = `{"type":"response_item","timestamp":"2026-11-03T10:00:00Z","payload":{"type":"function_call","name":"request_user_input","call_id":"k1","arguments":"{\"questions\":[{\"header\":\"Scope\",\"id\":\"scope_choice\",\"question\":\"Which scope?\",\"options\":[{\"label\":\"Small (Recommended)\",\"description\":\"one file\"},{\"label\":\"Large\",\"description\":\"all\"}]}]}"}}`

func TestCodexRequestUserInputPendingAndAnswered(t *testing.T) {
	b := feed(codexParser{}, codexAsk)
	p := snap(b)[0].Parts[0]
	if p.Status != "running" || len(p.Questions) != 1 || p.Questions[0].Header != "Scope" || len(p.Questions[0].Options) != 2 || p.Questions[0].MultiSelect {
		t.Fatalf("pending = %+v", p)
	}
	codexParser{}.Line([]byte(`{"type":"response_item","timestamp":"2026-11-03T10:00:05Z","payload":{"type":"function_call_output","call_id":"k1","output":"{\"answers\":{\"scope_choice\":{\"answers\":[\"Large\"]}}}"}}`), b)
	p = snap(b)[0].Parts[0]
	if p.Status != "ok" || p.Answers["Which scope?"] != "Large" {
		t.Fatalf("answered = %+v", p)
	}
}

func TestCodexRequestUserInputOutputWithoutAnswers(t *testing.T) {
	b := feed(codexParser{}, codexAsk,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"k1","output":"not json"}}`)
	if p := snap(b)[0].Parts[0]; p.Status != "ok" || p.Answers != nil || len(p.Questions) != 1 {
		t.Fatalf("part = %+v", p)
	}
}

// --- Other / Secret / optional options / opencode ---

func TestQuestionOtherFlag(t *testing.T) {
	if q := snap(feed(claudeParser{}, askUse))[0].Parts[0].Questions[0]; !q.Other {
		t.Fatalf("claude other = %+v", q)
	}
	if q := snap(feed(piParser{}, piAsk))[0].Parts[0].Questions[0]; !q.Other {
		t.Fatalf("pi ask_user_question other = %+v", q)
	}
	pc := snap(feed(piParser{},
		`{"type":"message","id":"p1","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"ask_user_choice","arguments":{"question":"Ship it?","options":[{"label":"Yes","description":"go","value":"y"}]}}]}}`))[0].Parts[0]
	if pc.Questions[0].Other {
		t.Fatalf("pi ask_user_choice other = %+v", pc.Questions[0])
	}
}

func TestCodexOptionalOptionsOtherAndSecret(t *testing.T) {
	b := feed(codexParser{},
		`{"type":"response_item","payload":{"type":"function_call","name":"request_user_input","call_id":"k1","arguments":"{\"questions\":[{\"id\":\"a\",\"header\":\"A\",\"question\":\"Pick?\",\"options\":[{\"label\":\"x\",\"description\":\"d\"}]},{\"id\":\"b\",\"header\":\"B\",\"question\":\"Pick or type?\",\"isOther\":true,\"options\":[{\"label\":\"y\",\"description\":\"d\"}]},{\"id\":\"c\",\"header\":\"C\",\"question\":\"Name?\"},{\"id\":\"d\",\"header\":\"D\",\"question\":\"Token?\",\"isSecret\":true}]}"}}`)
	qs := snap(b)[0].Parts[0].Questions
	if len(qs) != 4 {
		t.Fatalf("questions = %+v", qs)
	}
	if qs[0].Other || !qs[1].Other || !qs[2].Other || !qs[3].Other || qs[0].Secret || !qs[3].Secret {
		t.Fatalf("flags = %+v", qs)
	}
	if qs[2].Options == nil || len(qs[2].Options) != 0 {
		t.Fatalf("free-text options must be an empty array: %#v", qs[2].Options)
	}
	raw, _ := json.Marshal(qs[2])
	if !strings.Contains(string(raw), `"options":[]`) || strings.Contains(string(raw), `"secret"`) {
		t.Fatalf("json = %s", raw)
	}
	codexParser{}.Line([]byte(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"k1","output":"{\"answers\":{\"a\":{\"answers\":[\"x\"]},\"c\":{\"answers\":[\"Bob\"]},\"d\":{\"answers\":[\"hunter2\"]}}}"}}`), b)
	p := snap(b)[0].Parts[0]
	if p.Answers["Pick?"] != "x" || p.Answers["Name?"] != "Bob" || p.Answers["Token?"] != "••••" {
		t.Fatalf("answers = %v", p.Answers)
	}
	raw, _ = json.Marshal(p)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("secret leaked: %s", raw)
	}
}

func ocQuestionMsg(state string) (Message, bool) {
	return ocMessage(ocRow{id: "m1", typ: "assistant", created: 1000, data: []byte(
		`{"content":[{"type":"tool","id":"q1","name":"question","state":` + state + `}]}`)})
}

const ocQInput = `{"questions":[{"question":"Which db?","header":"DB","multiple":true,"options":[{"label":"pg","description":"Postgres"},{"label":"sqlite","description":"Embedded"}]},{"question":"Mode?","header":"Mode","options":[{"label":"fast","description":"f"}]}]}`

func TestOpenCodeQuestionPending(t *testing.T) {
	m, ok := ocQuestionMsg(`{"status":"running","input":` + ocQInput + `}`)
	if !ok {
		t.Fatal("no message")
	}
	p := m.Parts[0]
	if p.Status != "running" || len(p.Questions) != 2 || !p.Questions[0].MultiSelect || p.Questions[1].MultiSelect || !p.Questions[0].Other || p.Answers != nil {
		t.Fatalf("part = %+v", p)
	}
}

func TestOpenCodeQuestionAnswered(t *testing.T) {
	m, _ := ocQuestionMsg(`{"status":"completed","input":` + ocQInput + `,"content":[{"type":"text","text":"done"}],"metadata":{"answers":[["pg","sqlite"],[]]}}`)
	p := m.Parts[0]
	if p.Status != "ok" || p.Answers["Which db?"] != "pg, sqlite" || len(p.Answers) != 1 {
		t.Fatalf("part = %+v", p)
	}
}

func TestOpenCodeQuestionAnswersFromOutputText(t *testing.T) {
	m, _ := ocQuestionMsg(`{"status":"completed","input":` + ocQInput + `,"content":[{"type":"text","text":"{\"answers\":[[\"pg\"],[\"fast\"]]}"}]}`)
	p := m.Parts[0]
	if p.Answers["Which db?"] != "pg" || p.Answers["Mode?"] != "fast" {
		t.Fatalf("part = %+v", p)
	}
}

func TestOpenCodeQuestionMalformedFallsBack(t *testing.T) {
	m, _ := ocQuestionMsg(`{"status":"running","input":{"questions":"x"}}`)
	if p := m.Parts[0]; p.Type != "tool" || p.Questions != nil {
		t.Fatalf("part = %+v", p)
	}
}
