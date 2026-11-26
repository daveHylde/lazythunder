package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderKeyAndModel(t *testing.T) {
	var auth string
	var req map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"summary\":\"ok\",\"actions\":[],\"links\":[]}"}}]}`)
	}))
	defer srv.Close()
	a := &Agent{URL: srv.URL, HTTP: srv.Client()}
	if s, err := a.Summarize(context.Background(), "a", "b", "c", nil); err != nil || s.Summary != "ok" {
		t.Fatalf("%v %v", s, err)
	}
	if _, has := req["model"]; has || auth != "" {
		t.Fatalf("no model/key configured, sent model=%v auth=%q", req["model"], auth)
	}
	a.Model, a.APIKey = "gpt-x", "sk-test"
	a.Summarize(context.Background(), "a", "b", "c", nil)
	if req["model"] != "gpt-x" || auth != "Bearer sk-test" {
		t.Fatalf("model=%v auth=%q", req["model"], auth)
	}
	a.URL = "off"
	if _, err := a.Summarize(context.Background(), "a", "b", "c", nil); err == nil {
		t.Fatal("AI off still called out")
	}
}
