package ui

import (
	"testing"

	"github.com/daveHylde/lazythunder/internal/tb"
)

func TestSelectionBulkRead(t *testing.T) {
	m := Model{msgs: []tb.Message{{ID: 1}, {ID: 2, Read: true}, {ID: 3}}}
	mm, _ := m.keyMessages(" ") // select 1, cursor moves to 2
	m = mm.(Model)
	mm, _ = m.keyMessages("j") // skip 2
	m = mm.(Model)
	mm, _ = m.keyMessages(" ") // select 3
	m = mm.(Model)
	if len(m.marked) != 2 || !m.marked[1] || !m.marked[3] {
		t.Fatalf("marked = %v", m.marked)
	}
	if _, ids := m.targets(); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("targets = %v", ids)
	}
	mm, _ = m.keyMessageActions("m", nil)
	m = mm.(Model)
	if !m.msgs[0].Read || m.msgs[1].Read != true || !m.msgs[2].Read {
		t.Fatalf("after m: %+v", m.msgs)
	}
	mm, _ = m.keyMessageActions("m", nil) // all read now: toggles back to unread
	m = mm.(Model)
	if m.msgs[0].Read || !m.msgs[1].Read || m.msgs[2].Read {
		t.Fatalf("after second m: %+v", m.msgs)
	}
}

func TestDeletePromptNamesSelectedMessage(t *testing.T) {
	m := Model{msgs: []tb.Message{{ID: 1, Subject: "picked"}, {ID: 2, Subject: "cursor"}}}
	mm, _ := m.keyMessages(" ") // select 1; cursor moves to 2
	mm, _ = mm.(Model).keyMessageActions("d", nil)
	if got := mm.(Model).confirm.text; got != `Move "picked" to trash?` {
		t.Fatalf("prompt = %q", got)
	}
}
