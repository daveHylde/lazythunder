// Package hub is the native-messaging host Thunderbird spawns for the
// lazythunder add-on (see addon/PROTOCOL.md). It relays between the add-on's
// stdio pipe (length-prefixed JSON) and lazythunder clients on a unix socket
// (newline-delimited JSON), rewriting request ids so clients share the pipe
// and copying events to every client.
package hub

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// AddonID is the add-on's id; Thunderbird passes it as the host's second argument.
const AddonID = "lazythunder@lazythunder.local"

// maxFrame bounds one add-on message so a corrupt length can't exhaust memory.
const maxFrame = 1 << 30

// ReadFrame reads one native-messaging message: a 4-byte little-endian length, then JSON.
func ReadFrame(r io.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes is too large", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// WriteFrame writes msg as one native-messaging message.
func WriteFrame(w io.Writer, msg []byte) error {
	b := make([]byte, 4+len(msg))
	binary.LittleEndian.PutUint32(b, uint32(len(msg)))
	copy(b[4:], msg)
	_, err := w.Write(b)
	return err
}

// SocketPath is where the hub serves clients: $XDG_RUNTIME_DIR/lazythunder/hub.sock,
// else /tmp/lazythunder-$UID/hub.sock.
func SocketPath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "lazythunder", "hub.sock")
	}
	return filepath.Join(os.TempDir(), "lazythunder-"+strconv.Itoa(os.Getuid()), "hub.sock")
}

type pending struct {
	c  *client
	id json.RawMessage // the client's own id
}

// Hub relays between one add-on pipe and many clients.
type Hub struct {
	Logf func(format string, args ...any)

	pipeMu sync.Mutex
	pipe   io.Writer // nil once the add-on is gone (or was never there)

	mu      sync.Mutex
	nextID  int64
	pending map[int64]pending
	clients map[*client]struct{}
}

// New returns a hub writing requests to pipe; nil means no add-on is connected.
func New(pipe io.Writer) *Hub {
	return &Hub{Logf: log.Printf, pipe: pipe, pending: map[int64]pending{}, clients: map[*client]struct{}{}}
}

type client struct {
	conn net.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func (c *client) close() {
	c.once.Do(func() { close(c.done); c.conn.Close() })
}

// send queues one line; a client too slow to keep up with 256 messages is dropped
// rather than stalling the pipe for everyone.
func (c *client) send(line []byte) {
	select {
	case <-c.done:
	case c.out <- line:
	default:
		c.close()
	}
}

func (c *client) writeLoop() {
	w := bufio.NewWriter(c.conn)
	for {
		select {
		case <-c.done:
			return
		case line := <-c.out:
			w.Write(line)
			w.WriteByte('\n')
			for more := true; more; { // batch whatever else is queued
				select {
				case l := <-c.out:
					w.Write(l)
					w.WriteByte('\n')
				default:
					more = false
				}
			}
			if err := w.Flush(); err != nil {
				c.close()
				return
			}
		}
	}
}

func failure(id json.RawMessage, code, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{"id": id, "ok": false, "code": code, "error": msg})
	return b
}

// ServeConn relays one client's requests until it disconnects.
func (h *Hub) ServeConn(conn net.Conn) {
	c := &client{conn: conn, out: make(chan []byte, 256), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		for id, p := range h.pending {
			if p.c == c {
				delete(h.pending, id)
			}
		}
		h.mu.Unlock()
		c.close()
	}()
	go c.writeLoop()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			h.request(c, line)
		}
		if err != nil {
			return
		}
	}
}

func (h *Hub) request(c *client, line []byte) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(line, &msg); err != nil || msg == nil { // "null" decodes to a nil map
		if len(line) > 1 { // tolerate blank lines
			c.send(failure(nil, "BAD_REQUEST", "request is not a JSON object"))
		}
		return
	}
	h.mu.Lock()
	h.nextID++
	hid := h.nextID
	h.pending[hid] = pending{c: c, id: msg["id"]}
	h.mu.Unlock()
	msg["id"] = json.RawMessage(strconv.FormatInt(hid, 10))
	b, _ := json.Marshal(msg)

	h.pipeMu.Lock()
	err := errors.New("no add-on connected")
	if h.pipe != nil {
		if err = WriteFrame(h.pipe, b); err != nil {
			h.pipe = nil
		}
	}
	h.pipeMu.Unlock()
	if err != nil {
		h.mu.Lock()
		p, ok := h.pending[hid]
		delete(h.pending, hid)
		h.mu.Unlock()
		if ok {
			c.send(failure(p.id, "EXTENSION_DISCONNECTED", "Thunderbird add-on not connected: "+err.Error()))
		}
	}
}

// ReadPipe relays the add-on's responses and events until r fails (EOF:
// Thunderbird closed the port). Afterwards every request, pending or new, is
// answered EXTENSION_DISCONNECTED.
func (h *Hub) ReadPipe(r io.Reader) error {
	defer h.disconnect()
	for {
		b, err := ReadFrame(r)
		if err != nil {
			return err
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			h.Logf("bad message from add-on: %v", err)
			continue
		}
		if _, ok := msg["event"]; ok {
			line, _ := json.Marshal(msg) // compact: lines must not contain newlines
			h.mu.Lock()
			for c := range h.clients {
				c.send(line)
			}
			h.mu.Unlock()
			continue
		}
		hid, err := strconv.ParseInt(string(msg["id"]), 10, 64)
		if err != nil {
			h.Logf("add-on message without id or event: %.200s", b)
			continue
		}
		h.mu.Lock()
		p, ok := h.pending[hid]
		delete(h.pending, hid)
		h.mu.Unlock()
		if !ok {
			continue // its client left
		}
		msg["id"] = p.id
		if p.id == nil {
			msg["id"] = json.RawMessage("null")
		}
		line, _ := json.Marshal(msg)
		p.c.send(line)
	}
}

func (h *Hub) disconnect() {
	h.pipeMu.Lock()
	h.pipe = nil
	h.pipeMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, p := range h.pending {
		p.c.send(failure(p.id, "EXTENSION_DISCONNECTED", "Thunderbird add-on disconnected"))
		delete(h.pending, id)
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.close()
	}
}

// Listen serves path (its directory made private). A stale socket file is
// replaced; a live hub on it is replaced only when takeOver is set.
func Listen(path string, takeOver bool) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("%s is not a directory owned by us", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		if !takeOver {
			return nil, fmt.Errorf("another hub is serving %s", path)
		}
		log.Printf("taking over %s from a running hub", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(false) // a newer hub may own the path by the time we close
	return l, nil
}

// Run serves path until ctx ends or, with an add-on pipe (stdin non-nil),
// until the pipe closes.
func Run(ctx context.Context, path string, stdin io.Reader, stdout io.Writer, takeOver bool) error {
	l, err := Listen(path, takeOver)
	if err != nil {
		return err
	}
	ours, _ := os.Stat(path)
	defer func() {
		if cur, err := os.Stat(path); err == nil && os.SameFile(cur, ours) {
			os.Remove(path)
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var h *Hub
	if stdin != nil {
		h = New(stdout)
		go func() {
			err := h.ReadPipe(stdin)
			log.Printf("add-on pipe closed: %v", err)
			cancel()
		}()
	} else {
		h = New(nil)
	}
	defer h.Close()
	go func() { <-ctx.Done(); l.Close() }()
	log.Printf("serving %s (add-on pipe: %v)", path, stdin != nil)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go h.ServeConn(conn)
	}
}

// Main runs the hub process: from Thunderbird (stdio is the add-on's pipe,
// and the newest such hub takes the socket over) or by hand (`lazythunder hub`,
// where a terminal or /dev/null on stdin means no add-on). It logs to
// $XDG_STATE_HOME/lazythunder/hub.log because stdout is the pipe.
func Main(fromThunderbird bool) error {
	logDir := filepath.Join(stateDir(), "lazythunder")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, err := os.OpenFile(filepath.Join(logDir, "hub.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer f.Close()
			log.SetOutput(f)
		}
	}
	log.SetPrefix(fmt.Sprintf("hub[%d] ", os.Getpid()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var stdin io.Reader = os.Stdin
	if fi, err := os.Stdin.Stat(); !fromThunderbird && (err != nil || fi.Mode()&os.ModeCharDevice != 0) {
		stdin = nil
	}
	err := Run(ctx, SocketPath(), stdin, os.Stdout, fromThunderbird)
	if err != nil {
		log.Print(err)
	}
	return err
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}
