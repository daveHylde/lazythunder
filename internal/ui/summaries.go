package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/davidhyldgaard/lazythunder/internal/agent"
	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

type sumState int

const (
	sumNone sumState = iota
	sumQueued
	sumRunning
	sumDone
	sumFailed
)

type sumJob struct {
	id  int
	hdr string
}

// summarizer writes short AI summaries one at a time in the background,
// cached on disk by Message-ID. New mail is queued at the back; a message the
// user rests on jumps to the front.
type summarizer struct {
	mu      sync.Mutex
	cache   map[string]agent.Summary
	failed  map[string]string // hdr -> error
	queue   []sumJob
	queued  map[string]bool
	running string
	seen    map[string]bool // inbox messages the poller already knows
	polled  map[string]bool // inboxes with a first successful search (their baseline)
	wake    chan struct{}
	path    string
	paused  atomic.Bool // the chat agent is using the model: don't compete for its single slot
	ag      *agent.Agent
	c       *tb.Client
}

type (
	summaryMsg struct{ hdr string }
	pollTick   struct{}
	newMailMsg struct{ n int }
)

func newSummarizer(ag *agent.Agent, c *tb.Client) *summarizer {
	s := &summarizer{cache: map[string]agent.Summary{}, failed: map[string]string{}, queued: map[string]bool{},
		seen: map[string]bool{}, polled: map[string]bool{}, wake: make(chan struct{}, 1), ag: ag, c: c, path: filepath.Join(cacheDir(), "summaries-v2.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.cache)
	}
	return s
}

func (s *summarizer) get(hdr string) (agent.Summary, sumState, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case hdr == "":
		return agent.Summary{}, sumNone, ""
	case s.cache[hdr].Summary != "":
		return s.cache[hdr], sumDone, ""
	case s.running == hdr:
		return agent.Summary{}, sumRunning, ""
	case s.queued[hdr]:
		return agent.Summary{}, sumQueued, ""
	case s.failed[hdr] != "":
		return agent.Summary{}, sumFailed, s.failed[hdr]
	}
	return agent.Summary{}, sumNone, ""
}

// enqueue adds a message unless it is done or pending; front moves it ahead.
func (s *summarizer) enqueue(id int, hdr string, front bool) {
	if hdr == "" {
		return
	}
	s.mu.Lock()
	if s.cache[hdr].Summary != "" || s.running == hdr {
		s.mu.Unlock()
		return
	}
	delete(s.failed, hdr) // an explicit retry
	if s.queued[hdr] {
		if front {
			for i, j := range s.queue {
				if j.hdr == hdr {
					s.queue = append(s.queue[:i], s.queue[i+1:]...)
					break
				}
			}
			s.queue = append([]sumJob{{id, hdr}}, s.queue...)
		}
		s.mu.Unlock()
		return
	}
	s.queued[hdr] = true
	if front {
		s.queue = append([]sumJob{{id, hdr}}, s.queue...)
	} else {
		s.queue = append(s.queue, sumJob{id, hdr})
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run processes the queue until ctx ends; notify tells the UI a summary changed.
func (s *summarizer) run(ctx context.Context, notify func(string)) {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 || s.paused.Load() {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(2 * time.Second): // re-check after the agent finishes
			}
			continue
		}
		job := s.queue[0]
		s.queue = s.queue[1:]
		delete(s.queued, job.hdr)
		s.running = job.hdr
		s.mu.Unlock()
		notify(job.hdr)

		sum, err := s.summarize(ctx, job.id)

		if err == nil && sum.Summary == "" {
			err = fmt.Errorf("empty summary")
		}
		s.mu.Lock()
		s.running = ""
		var snapshot []byte
		if err != nil {
			s.failed[job.hdr] = err.Error()
		} else {
			s.cache[job.hdr] = sum
			snapshot, _ = json.Marshal(s.cache)
		}
		s.mu.Unlock()
		if snapshot != nil {
			s.save(snapshot) // file IO outside the lock: View reads the cache meanwhile
		}
		notify(job.hdr)
	}
}

func (s *summarizer) summarize(ctx context.Context, id int) (agent.Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	m, err := s.c.Read(ctx, id)
	if err != nil {
		return agent.Summary{}, err
	}
	// the same numbering as the preview (PreferHTML, safeLink), so [n] lines up
	var links []string
	body := render.Body(m.Parts.Text, m.Parts.HTML, render.Opts{Links: true, PreferHTML: true, SafeLink: safeLink,
		OnLink: func(u, label string) { links = append(links, strings.TrimSpace(oneLine(label)+" → "+u)) }})
	return s.ag.Summarize(ctx, m.Author, m.Subject, body, links)
}

// save writes the cache atomically, owner-only: summaries describe mail.
// Only the run goroutine saves, so writes never interleave.
func (s *summarizer) save(b []byte) {
	if os.MkdirAll(filepath.Dir(s.path), 0o700) != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

// pollNewMail looks for inbox messages that arrived since the last poll and
// queues them for summaries. The first poll only records what is already
// there: mail that predates the session is not backfilled.
func (m Model) pollNewMail() tea.Cmd {
	s, c, ctx := m.sum, m.c, m.ctx
	var inboxes []string
	for _, f := range m.foldersOf("") {
		inboxes = append(inboxes, f.ID)
	}
	return func() tea.Msg {
		since := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339) // Date headers can lag arrival
		var fresh []tb.Message
		for _, id := range inboxes {
			p, err := c.Search(ctx, "", tb.SearchOpts{Folder: id, Since: since, Limit: 500})
			if err != nil {
				continue // this inbox gets its baseline on a later poll
			}
			s.mu.Lock()
			baseline := !s.polled[id] // first successful look: record, don't queue the backlog
			s.polled[id] = true
			for _, x := range p.Messages {
				if x.HeaderMessageID != "" && !s.seen[x.HeaderMessageID] {
					s.seen[x.HeaderMessageID] = true
					if !baseline {
						fresh = append(fresh, x)
					}
				}
			}
			s.mu.Unlock()
		}
		for _, x := range fresh {
			s.enqueue(x.ID, x.HeaderMessageID, false)
		}
		return newMailMsg{len(fresh)}
	}
}

func nextPoll() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return pollTick{} })
}
