// lazythunder: a lazygit-style terminal UI for Thunderbird, built on
// thunderbird-cli (`tb`) with a local llama.cpp mail assistant.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/bg"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/ui"
)

const usage = "usage: lazythunder [daemon]"

func main() {
	c := tb.New()
	run := func() error { return ui.Run(c, agent.New(c)) }
	switch arg := strings.Join(os.Args[1:], " "); arg {
	case "":
	case "daemon":
		run = func() error { return daemon(c, agent.New(c)) }
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lazythunder:", err)
		os.Exit(1)
	}
}

// daemon writes notes, reply status and unsubscribe methods as mail
// arrives, while the TUI is closed too.
func daemon(c *tb.Client, ag *agent.Agent) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path, err := store.DefaultPath()
	if err != nil {
		return err
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ImportJSON(bg.LegacyCache()); err != nil {
		log.Printf("import old summary cache: %v", err)
	}
	sv := &supervisor{c: c, st: st}
	sv.beat() // not up yet: clears a beat left by a daemon that died
	go sv.heartbeat(ctx)
	defer sv.setUp(false) // TUIs take over at once
	if err := ensure(ctx, c); err != nil {
		return nil // only a signal ends ensure
	}
	sv.setUp(true)
	log.Printf("connected to Thunderbird; AI %v", ag.Enabled())
	w := &bg.Worker{Store: st, Mail: c, AI: ag, Name: fmt.Sprintf("daemon-%d", os.Getpid()), Logf: log.Printf,
		Offline: func(err error) bool { return sv.reconnect(ctx, err) }}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, nil) }()
	poll(ctx, c, sv, ag.Enabled(), w.Wake)
	<-done
	log.Print("stopped")
	return nil
}

// supervisor tracks whether the daemon can work (Thunderbird reachable) and
// brings Thunderbird back when it goes away.
type supervisor struct {
	c  *tb.Client
	st *store.Store
	up atomic.Bool
	mu sync.Mutex // one recovery at a time
}

// beat tells TUIs whether a daemon is doing the background work: the time
// while up, else 0 so they do it themselves.
func (s *supervisor) beat() {
	v := "0"
	if s.up.Load() {
		v = strconv.FormatInt(time.Now().Unix(), 10)
	}
	if err := s.st.SetKV(bg.KeyHeartbeat, v); err != nil {
		log.Printf("heartbeat: %v", err)
	}
}

func (s *supervisor) setUp(up bool) {
	s.up.Store(up)
	s.beat()
}

func (s *supervisor) heartbeat(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			s.beat()
		}
	}
}

// reconnect checks, after a failure err, whether Thunderbird stopped
// answering; if so it stops the heartbeat and waits for ensure to restart
// it (headless). It reports whether Thunderbird was down.
func (s *supervisor) reconnect(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.Run(ctx, nil, "health") == nil {
		return false // a real failure, or another caller already recovered
	}
	log.Printf("Thunderbird went away (%v); restarting it", err)
	s.setUp(false)
	if ensure(ctx, s.c) == nil {
		s.setUp(true)
		log.Print("reconnected to Thunderbird")
	}
	return true
}

// ensure waits for Thunderbird, backing off from 5s to 5min between tries.
func ensure(ctx context.Context, c *tb.Client) error {
	for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
		err := c.Ensure(ctx, func(s string) { log.Print(s) })
		if err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("Thunderbird unreachable: %v (retrying in %s)", err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// poll runs a poller pass every minute until ctx ends; folders are reloaded
// each pass so new accounts are picked up.
func poll(ctx context.Context, c *tb.Client, sv *supervisor, ai bool, wake func()) {
	p := bg.Poller{Store: sv.st, Mail: c, AI: ai}
	for {
		fs, err := c.Folders(ctx)
		if err != nil {
			log.Printf("folders: %v", err)
		} else {
			var n int
			n, err = p.Pass(ctx, fs)
			if err != nil {
				log.Printf("poll: %v", err)
			}
			if n > 0 {
				log.Printf("%d new message(s)", n)
				wake()
			}
		}
		if sv.reconnect(ctx, err) {
			continue // poll again right away: mail may have arrived meanwhile
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
