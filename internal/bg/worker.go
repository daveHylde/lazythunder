// Package bg produces notes, reply status and unsubscribe methods in the
// background, for both the TUI and the daemon, storing them in the store.
package bg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/render"
	"github.com/daveHylde/lazythunder/internal/store"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/unsub"
)

// KV keys shared by the TUI and the daemon.
const (
	KeyPauseUntil = "pause_until"      // unix seconds: workers don't claim before then
	KeyHeartbeat  = "daemon_heartbeat" // unix seconds of the daemon's last sign of life; 0 while it can't work
)

// DaemonAlive reports whether heartbeat value v means a daemon is doing the
// background work at now. It beats every 30s, so 2 minutes of silence is death.
func DaemonAlive(v string, now time.Time) bool {
	t, err := strconv.ParseInt(v, 10, 64)
	return err == nil && t > 0 && now.Sub(time.Unix(t, 0)) <= 2*time.Minute
}

// Mail is the part of tb the background work needs (*tb.Client).
type Mail interface {
	Search(ctx context.Context, query string, o tb.SearchOpts) (tb.Page, error)
	Read(ctx context.Context, id int) (tb.FullMessage, error)
	Headers(ctx context.Context, id int) (mail.Header, error)
	ByMessageID(ctx context.Context, hdr string) ([]tb.Message, error)
}

// AI is the part of the agent the worker needs (*agent.Agent).
type AI interface {
	Enabled() bool
	Summarize(ctx context.Context, from, subject, body string, links []string) (agent.Summary, error)
}

// Worker processes the store's job queue one message at a time.
type Worker struct {
	Store *store.Store
	Mail  Mail
	AI    AI
	Name  string               // identifies the claimer in the jobs table
	Logf  func(string, ...any) // nil: quiet
	// Offline (nil: never) reports whether a job failed because the mail
	// source was unreachable, and may block until it is back. Such a job is
	// requeued instead of counted as a failed attempt.
	Offline func(error) bool
	once    sync.Once
	wake    chan struct{}
}

// LegacyCache is the JSON summary cache that the store replaced.
func LegacyCache() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "lazythunder", "summaries-v2.json")
}

// Wake makes an idle Run look for work now instead of at its next poll.
func (w *Worker) Wake() {
	select {
	case w.wakeCh() <- struct{}{}:
	default:
	}
}

func (w *Worker) wakeCh() chan struct{} {
	w.once.Do(func() { w.wake = make(chan struct{}, 1) })
	return w.wake
}

func (w *Worker) logf(f string, a ...any) {
	if w.Logf != nil {
		w.Logf(f, a...)
	}
}

// Run processes jobs until ctx ends. active (nil: always) is checked before
// every claim, so the worker can stand aside, e.g. while a daemon runs.
func (w *Worker) Run(ctx context.Context, active func() bool) {
	wake := w.wakeCh()
	for ctx.Err() == nil {
		if active == nil || active() {
			did, err := w.Step(ctx)
			if err != nil {
				w.logf("worker: %v", err)
			}
			if did {
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-time.After(2 * time.Second):
		}
	}
}

// Step claims and processes one job. did is false when there was nothing to
// do: AI off, paused, or an empty queue.
func (w *Worker) Step(ctx context.Context) (did bool, err error) {
	if !w.AI.Enabled() {
		return false, nil
	}
	if paused, err := Paused(w.Store); paused || err != nil {
		return false, err
	}
	hdr, id, ok, err := w.Store.Claim(w.Name)
	if !ok || err != nil {
		return false, err
	}
	jobErr := w.process(ctx, hdr, id)
	if jobErr != nil && (ctx.Err() != nil || w.Offline != nil && w.Offline(jobErr)) { // not the message's fault: leave it for the next try
		return true, errors.Join(w.Store.Finish(hdr, jobErr), w.Store.Enqueue(hdr, id, 0))
	}
	if jobErr != nil {
		w.logf("note %s: %v", hdr, jobErr)
	}
	return true, w.Store.Finish(hdr, jobErr)
}

// Paused reports whether the chat agent asked workers to leave the model alone.
func Paused(s *store.Store) (bool, error) {
	v, ok, err := s.KV(KeyPauseUntil)
	if !ok || err != nil {
		return false, err
	}
	until, _ := strconv.ParseInt(v, 10, 64)
	return time.Now().Unix() < until, nil
}

func (w *Worker) process(ctx context.Context, hdr string, id int) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	m, err := w.Mail.Read(ctx, id)
	// Message ids are only stable while Thunderbird runs: after a restart id
	// may be gone or another message, so find the message again by Message-ID.
	if tb.Code(err) == "NOT_FOUND" || err == nil && m.HeaderMessageID != hdr {
		ms, ferr := w.Mail.ByMessageID(ctx, hdr)
		if ferr != nil {
			return ferr
		}
		if len(ms) == 0 {
			return fmt.Errorf("message %s no longer exists", hdr)
		}
		m, err = w.Mail.Read(ctx, ms[0].ID)
	}
	if err != nil {
		return err
	}
	body, links := summaryInput(m)
	sum, err := w.AI.Summarize(ctx, m.Author, m.Subject, body, links)
	if err == nil && sum.Summary == "" {
		err = errors.New("empty summary")
	}
	if err != nil {
		return err
	}
	data, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	if err := w.Store.PutNote(hdr, data, known(sum.Category, agent.Categories), known(sum.Priority, priorities)); err != nil {
		return err
	}
	if err := w.storeUnsub(ctx, hdr, m); err != nil { // the note is in: don't fail the job over this
		w.logf("unsubscribe %s: %v", hdr, err)
	}
	return nil
}

var priorities = []string{"high", "normal", "low"}

// known returns v if it is one of allowed, else "": the model may invent values.
func known(v string, allowed []string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return ""
}

// summaryInput renders the body with the same numbering as the preview
// (PreferHTML, SafeLink), so the note's [n] line up with what the user sees.
func summaryInput(m tb.FullMessage) (body string, links []string) {
	body = render.Body(m.Parts.Text, m.Parts.HTML, render.Opts{Links: true, PreferHTML: true, SafeLink: SafeLink,
		OnLink: func(u, label string) { links = append(links, strings.TrimSpace(OneLine(label)+" → "+u)) }})
	return body, links
}

// UnsubLinks lists every body link (url, label), unfiltered: unsubscribe
// links are exactly what SafeLink hides.
func UnsubLinks(m tb.FullMessage) [][2]string {
	var links [][2]string
	render.Body(m.Parts.Text, m.Parts.HTML, render.Opts{PreferHTML: true,
		OnLink: func(u, label string) { links = append(links, [2]string{u, label}) }})
	return links
}

func (w *Worker) storeUnsub(ctx context.Context, hdr string, m tb.FullMessage) error {
	h, err := w.Mail.Headers(ctx, m.ID)
	if err != nil {
		return err
	}
	methods := unsub.Methods(h, UnsubLinks(m))
	if methods == nil {
		methods = []unsub.Method{} // stored as [] : looked, found none
	}
	data, err := json.Marshal(methods)
	if err != nil {
		return err
	}
	return w.Store.PutUnsub(hdr, data)
}
