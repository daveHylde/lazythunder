package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daveHylde/lazythunder/internal/tb"
)

// fakeLLM replies with the scripted assistant messages in order and records requests.
func fakeLLM(t *testing.T, replies ...string) (*httptest.Server, *[][]Msg) {
	var seen [][]Msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []Msg }
		json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req.Messages)
		if len(seen) > len(replies) {
			t.Fatalf("unexpected request %d", len(seen))
		}
		w.Write([]byte(`{"choices":[{"message":` + replies[len(seen)-1] + `}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestAskDeclinedActionAndDraft(t *testing.T) {
	srv, seen := fakeLLM(t,
		`{"content":"","tool_calls":[
			{"id":"1","type":"function","function":{"name":"delete","arguments":"{\"ids\":[5]}"}},
			{"id":"2","type":"function","function":{"name":"propose_draft","arguments":"{\"kind\":\"reply\",\"message_id\":5,\"body\":\"Takk!\"}"}}]}`,
		`{"content":"Done."}`)
	a := &Agent{URL: srv.URL, Model: "m", TB: &tb.Client{Bin: "false"}, HTTP: srv.Client(), accounts: "- test\n"}

	var asked string
	var proposed tb.Draft
	text, err := a.Ask(context.Background(), "clean up", "folder x", Hooks{
		Confirm: func(s string) bool { asked = s; return false },
		Propose: func(d tb.Draft) { proposed = d },
		Status:  func(string) {},
	})
	if err != nil || text != "Done." {
		t.Fatalf("got %q, %v", text, err)
	}
	if asked != "move [5] to trash" {
		t.Fatalf("confirm text %q", asked)
	}
	if proposed != (tb.Draft{Kind: tb.Reply, ReplyTo: 5, Body: "Takk!"}) {
		t.Fatalf("proposed %+v", proposed)
	}
	second := (*seen)[1]
	if got := second[len(second)-2].Content; !strings.Contains(got, "NOT DONE") {
		t.Fatalf("delete tool result %q", got)
	}
	if !strings.Contains(second[0].Content, "Current view: folder x") {
		t.Fatalf("system prompt missing view: %q", second[0].Content)
	}
}

func TestTrimKeepsSystemAndDropsOrphanToolResults(t *testing.T) {
	big := strings.Repeat("x", 40000)
	a := &Agent{History: []Msg{{Role: "system"}, {Role: "assistant", Content: big}, {Role: "tool", Content: big}, {Role: "user", Content: "hi"}}}
	a.trim()
	if len(a.History) != 2 || a.History[0].Role != "system" || a.History[1].Content != "hi" {
		t.Fatalf("got %+v", a.History)
	}
}
