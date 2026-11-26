package tb

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type handler func(op string, params json.RawMessage) (any, *Error)

// fakeHub serves h on a temp socket, answering each request concurrently,
// and returns a Client dialing it. send pushes a raw line to every connection.
func fakeHubConns(t *testing.T, h handler) (*Client, func(line string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, nc)
			mu.Unlock()
			var wmu sync.Mutex
			write := func(v any) {
				b, _ := json.Marshal(v)
				wmu.Lock()
				nc.Write(append(b, '\n'))
				wmu.Unlock()
			}
			go func() {
				r := bufio.NewReader(nc)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     json.RawMessage
						Op     string
						Params json.RawMessage
					}
					json.Unmarshal(line, &req)
					go func() {
						data, e := h(req.Op, req.Params)
						if e != nil {
							write(map[string]any{"id": req.ID, "ok": false, "error": e.Msg, "code": e.Code})
						} else {
							write(map[string]any{"id": req.ID, "ok": true, "data": data})
						}
					}()
				}
			}()
		}
	}()
	return &Client{Socket: path}, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Write([]byte(line + "\n"))
		}
	}
}

func fakeHub(t *testing.T, h handler) *Client {
	c, _ := fakeHubConns(t, h)
	return c
}

func TestCallMultiplexes(t *testing.T) {
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		var p struct{ ID int }
		json.Unmarshal(params, &p)
		time.Sleep(time.Duration(20-p.ID) * time.Millisecond) // answer out of order
		return Message{ID: p.ID, Subject: op}, nil
	})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := c.Read(context.Background(), i)
			if err != nil || m.ID != i || m.Subject != "read" {
				t.Errorf("read %d: %+v %v", i, m.Message, err)
			}
		}()
	}
	wg.Wait()
}

func TestCallErrors(t *testing.T) {
	c := fakeHub(t, func(op string, _ json.RawMessage) (any, *Error) {
		if op == "health" {
			return nil, &Error{Code: "NOT_FOUND", Msg: "nope"}
		}
		select {} // never answers
	})
	if err := c.Health(context.Background()); Code(err) != "NOT_FOUND" || err.Error() != "nope (NOT_FOUND)" {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Folders(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: got %v", err)
	}
	gone := &Client{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if err := gone.Health(context.Background()); Code(err) != "EXTENSION_DISCONNECTED" {
		t.Fatalf("dial failure: got %v", err)
	}
}

func TestRedialsAfterHubRestart(t *testing.T) {
	c := fakeHub(t, func(string, json.RawMessage) (any, *Error) { return nil, nil })
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.conn.nc.Close() // hub went away
	dead := c.conn.dead
	c.mu.Unlock()
	<-dead
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("no redial: %v", err)
	}
}

func TestOpsParams(t *testing.T) {
	got := map[string]string{}
	var mu sync.Mutex
	c := fakeHub(t, func(op string, params json.RawMessage) (any, *Error) {
		mu.Lock()
		got[op] = string(params)
		mu.Unlock()
		switch op {
		case "download":
			return map[string]string{"data": base64.StdEncoding.EncodeToString([]byte("PDF!"))}, nil
		case "headers":
			return map[string]string{"raw": "List-Unsubscribe: <https://x>\r\nSubject: hi\r\n\r\nbody"}, nil
		}
		return nil, nil
	})
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.pdf")
	if err := c.Download(ctx, 5, "1.2", path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "PDF!" {
		t.Fatalf("download wrote %q", b)
	}
	h, err := c.Headers(ctx, 5)
	if err != nil || h.Get("List-Unsubscribe") != "<https://x>" {
		t.Fatalf("headers %v %v", h, err)
	}
	c.MarkFolderRead(ctx, "account1://INBOX")
	c.Mark(ctx, []int{1, 2}, "junk")
	c.Submit(ctx, Draft{Kind: ReplyAll, ReplyTo: 9, Body: "ok", To: "ignored"}, true)
	c.Search(ctx, "", SearchOpts{Folder: "f", Unread: true, Limit: 5})
	for op, want := range map[string]string{
		"download":       `{"id":5,"part":"1.2"}`,
		"headers":        `{"id":5}`,
		"markFolderRead": `{"folder":"account1://INBOX"}`,
		"mark":           `{"flag":"junk","ids":[1,2]}`,
		"submit":         `{"body":"ok","kind":"replyAll","replyTo":9,"send":true}`,
		"search":         `{"folder":"f","limit":5,"unread":true}`,
	} {
		if got[op] != want {
			t.Errorf("%s params %s, want %s", op, got[op], want)
		}
	}
}

func TestEvents(t *testing.T) {
	c, send := fakeHubConns(t, func(string, json.RawMessage) (any, *Error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for ev := (Event{}); ev.Name == ""; {
		send(`{"id":3,"ok":true}`) // replies are not events
		send(`{"event":"newMail","data":{"folder":"account1://INBOX"}}`)
		select {
		case ev = <-ch:
			if ev.Name != "newMail" || string(ev.Data) != `{"folder":"account1://INBOX"}` {
				t.Fatalf("got %+v", ev)
			}
		case <-time.After(50 * time.Millisecond): // connection may not be accepted yet
		}
	}
	cancel()
	for range ch { // must close
	}
	gone := &Client{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if _, err := gone.Events(context.Background()); Code(err) != "EXTENSION_DISCONNECTED" {
		t.Fatalf("got %v", err)
	}
}
