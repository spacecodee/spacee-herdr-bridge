package chat

// Rewinds. Agents keep abandoned branches in the transcript (Claude Code and pi link
// records into a tree; Codex appends a rollback marker). The Builder shows only the live
// branch and, while tailing, removes what a late rewind abandons.

const maxTurns = 256 // turns remembered for Codex rollbacks

// Node registers the transcript record about to be parsed: id, its parent, whether it can
// be the end of the live branch (leaf) and whether it can start a new one (fork). Call it
// before Add for each record (with an empty id for records outside the tree). The live
// branch is the ancestry of the newest leaf record. When a fork record hangs off an older
// live record, everything after that record is dropped; other records hanging off an older
// one (the results of parallel tool calls) just add a branch that stays live. Only that
// positive evidence prunes: an unknown parent never does.
func (b *Builder) Node(id, parent string, leaf, fork bool) {
	b.cur = id
	if id == "" {
		return
	}
	b.nodes[id] = parent
	if !leaf {
		// Attachments and notes hang off the conversation: live while what they hang off is.
		if _, known := b.nodes[parent]; parent == "" || !known || b.live[parent] {
			b.live[id] = true
		}
		return
	}
	path := []string{id}
	p := parent
	for steps := 0; p != "" && !b.live[p] && steps <= len(b.nodes); steps++ {
		next, ok := b.nodes[p]
		if !ok {
			p = ""
			break
		}
		path = append(path, p)
		p = next
	}
	rewind := fork && p != "" && b.live[p] && p != b.leaf
	b.leaf = id
	if rewind {
		b.live = map[string]bool{}
		b.markAncestors(id)
		b.prune()
		return
	}
	for _, n := range path {
		b.live[n] = true
	}
}

func (b *Builder) markAncestors(id string) {
	for steps := 0; id != "" && steps <= len(b.nodes); steps++ {
		b.live[id] = true
		id = b.nodes[id]
	}
}

// visible reports whether the record belongs to the live branch.
func (b *Builder) visible(id string) bool {
	_, known := b.nodes[id]
	return id == "" || !known || b.leaf == "" || b.live[id]
}

// own records that the current record contributed to message id.
func (b *Builder) own(id string) {
	if b.cur == "" {
		return
	}
	for _, o := range b.owners[id] {
		if o == b.cur {
			return
		}
	}
	b.owners[id] = append(b.owners[id], b.cur)
}

// prune removes the messages whose records all left the live branch.
func (b *Builder) prune() {
	var drop []string
	for _, m := range b.msgs {
		own := b.owners[m.ID]
		keep := len(own) == 0
		for _, o := range own {
			keep = keep || b.visible(o)
		}
		if !keep {
			drop = append(drop, m.ID)
		}
	}
	for _, id := range drop {
		b.remove(id)
	}
}

// StartTurn marks the start of a user turn (Codex): messages added until the next turn
// belong to it.
func (b *Builder) StartTurn() {
	b.turns = append(b.turns, nil)
	if len(b.turns) > maxTurns {
		b.turns = b.turns[len(b.turns)-maxTurns:]
	}
}

// Rollback drops the last n turns (Codex thread_rolled_back).
func (b *Builder) Rollback(n int) {
	for ; n > 0 && len(b.turns) > 0; n-- {
		last := b.turns[len(b.turns)-1]
		b.turns = b.turns[:len(b.turns)-1]
		for _, id := range last {
			b.remove(id)
		}
	}
}
