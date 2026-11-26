package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, m := range []string{`{"a":1}`, ``, `{"b":"ø"}`} {
		if err := WriteFrame(&buf, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	if got := buf.Bytes()[:4]; !bytes.Equal(got, []byte{7, 0, 0, 0}) {
		t.Fatalf("length prefix %v", got)
	}
	for _, want := range []string{`{"a":1}`, ``, `{"b":"ø"}`} {
		b, err := ReadFrame(&buf)
		if err != nil || string(b) != want {
			t.Fatalf("got %q, %v; want %q", b, err, want)
		}
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

type testClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func (c testClient) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (c testClient) recv(t *testing.T) map[string]any {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return m
}

// setup returns a hub wired to in-memory pipes: addonIn delivers frames to
// the hub as the add-on would, addonOut reads the frames it sends the add-on.
func setup(t *testing.T, n int) (h *Hub, addonIn *io.PipeWriter, addonOut *bufio.Reader, clients []testClient) {
	toHub, fromAddon := io.Pipe()
	toAddon, fromHub := io.Pipe()
	h = New(fromHub)
	h.Logf = t.Logf
	go h.ReadPipe(toHub)
	for range n {
		a, b := net.Pipe()
		go h.ServeConn(b)
		clients = append(clients, testClient{a, bufio.NewReader(a)})
	}
	t.Cleanup(func() { fromAddon.Close(); toAddon.Close(); h.Close() })
	return h, fromAddon, bufio.NewReader(toAddon), clients
}

func readReq(t *testing.T, r io.Reader) map[string]json.RawMessage {
	t.Helper()
	b, err := ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	return m
}

func TestIDRewriting(t *testing.T) {
	_, addonIn, addonOut, cs := setup(t, 2)
	cs[0].send(t, `{"id":7,"op":"read","params":{"id":42}}`)
	r1 := readReq(t, addonOut)
	cs[1].send(t, `{"id":7,"op":"health"}`)
	r2 := readReq(t, addonOut)
	if string(r1["op"]) != `"read"` || string(r1["params"]) != `{"id":42}` {
		t.Fatalf("request mangled: %v", r1)
	}
	if string(r1["id"]) == string(r2["id"]) {
		t.Fatalf("clients' ids not rewritten apart: both %s", r1["id"])
	}
	// answer out of order
	WriteFrame(addonIn, []byte(`{"id":`+string(r2["id"])+`,"ok":true,"data":{"version":"1"}}`))
	WriteFrame(addonIn, []byte(`{"id":`+string(r1["id"])+`,"ok":false,"error":"gone","code":"NOT_FOUND"}`))
	a := cs[0].recv(t)
	b := cs[1].recv(t)
	if a["id"] != 7.0 || a["code"] != "NOT_FOUND" || a["ok"] != false {
		t.Fatalf("client 0 got %v", a)
	}
	if b["id"] != 7.0 || b["ok"] != true || b["data"].(map[string]any)["version"] != "1" {
		t.Fatalf("client 1 got %v", b)
	}
}

func TestEventFanOut(t *testing.T) {
	h, addonIn, _, cs := setup(t, 3)
	for n := 0; n < 3; { // let ServeConn register the clients
		time.Sleep(5 * time.Millisecond)
		h.mu.Lock()
		n = len(h.clients)
		h.mu.Unlock()
	}
	WriteFrame(addonIn, []byte("{\"event\": \"newMail\",\n \"data\": {\"folder\": \"account1://INBOX\"}}"))
	for i, c := range cs {
		m := c.recv(t)
		if m["event"] != "newMail" || m["data"].(map[string]any)["folder"] != "account1://INBOX" {
			t.Fatalf("client %d got %v", i, m)
		}
	}
}

func TestDisconnected(t *testing.T) {
	_, addonIn, addonOut, cs := setup(t, 1)
	cs[0].send(t, `{"id":1,"op":"folders"}`)
	readReq(t, addonOut)
	addonIn.Close() // Thunderbird closed the port
	if m := cs[0].recv(t); m["id"] != 1.0 || m["code"] != "EXTENSION_DISCONNECTED" {
		t.Fatalf("pending request got %v", m)
	}
	cs[0].send(t, `{"id":2,"op":"health"}`)
	if m := cs[0].recv(t); m["id"] != 2.0 || m["code"] != "EXTENSION_DISCONNECTED" {
		t.Fatalf("new request got %v", m)
	}
}

func TestNoAddon(t *testing.T) {
	h := New(nil)
	a, b := net.Pipe()
	go h.ServeConn(b)
	defer a.Close()
	c := testClient{a, bufio.NewReader(a)}
	c.send(t, `null`) // decodes to a nil map: must be rejected, not panic
	if m := c.recv(t); m["code"] != "BAD_REQUEST" {
		t.Fatalf("null got %v", m)
	}
	c.send(t, `{"id":"x","op":"health"}`)
	if m := c.recv(t); m["id"] != "x" || m["code"] != "EXTENSION_DISCONNECTED" {
		t.Fatalf("got %v", m)
	}
}

func TestRunTakesOverAndExitsOnEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s", "hub.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := make(chan error, 1)
	go func() { old <- Run(ctx, path, nil, nil, false) }()
	waitDial(t, path)
	if _, err := Listen(path, false); err == nil || !strings.Contains(err.Error(), "another hub") {
		t.Fatalf("second hub without takeOver: %v", err)
	}
	stdin, addonIn := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), path, stdin, io.Discard, true) }()
	time.Sleep(100 * time.Millisecond)
	conn := waitDial(t, path)
	r := bufio.NewReader(conn)
	for got := false; !got; { // the hub registers the client after accepting it
		WriteFrame(addonIn, []byte(`{"event":"changed","data":{}}`))
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		line, err := r.ReadBytes('\n')
		if err == nil && !strings.Contains(string(line), `"changed"`) {
			t.Fatalf("not connected to the new hub: %s", line)
		}
		got = err == nil
	}
	cancel() // the old hub must not unlink the new one's socket
	<-old
	addonIn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not exit on EOF")
	}
}

func waitDial(t *testing.T, path string) net.Conn {
	t.Helper()
	for range 100 {
		if c, err := net.Dial("unix", path); err == nil {
			t.Cleanup(func() { c.Close() })
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hub never listened")
	return nil
}
