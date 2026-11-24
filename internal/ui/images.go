package ui

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
	"github.com/davidhyldgaard/lazythunder/internal/termimg"
)

const (
	maxImageBytes   = 10 << 20 // per image file
	maxImagePixels  = 25e6     // decompression-bomb guard, checked before decoding
	maxImagesPerMsg = 60
	cacheWidth      = 480 // decoded images are shrunk to this for inline drawing
)

type imgState int

const (
	imgPending imgState = iota + 1
	imgFailed
)

// imageStore caches decoded images per message and keeps the original files
// for the hi-res viewer. Shared by all Model copies, so it is mutex-guarded.
type imageStore struct {
	mu      sync.Mutex
	dir     string
	imgs    map[string]image.Image
	paths   map[string]string
	state   map[string]imgState
	order   map[int][]string // message id -> keys in document order
	seen    map[int]map[string]bool
	keyOf   map[string]string // placement token -> key
	sixel   map[string]string // token|cols|rows|cell size|crop -> encoded sixel ("" while encoding)
	noSixel map[string]bool   // keys whose sixel encode failed: shown as half-blocks
}

// newImageStore returns nil (images disabled) if no temp dir can be made.
func newImageStore() *imageStore {
	dir, err := os.MkdirTemp("", "lazythunder-img-")
	if err != nil {
		return nil
	}
	return &imageStore{dir: dir, imgs: map[string]image.Image{}, paths: map[string]string{},
		state: map[string]imgState{}, order: map[int][]string{}, seen: map[int]map[string]bool{},
		keyOf: map[string]string{}, sixel: map[string]string{}, noSixel: map[string]bool{}}
}

func (s *imageStore) close() {
	if s != nil {
		os.RemoveAll(s.dir)
	}
}

// imgKey scopes cid: references to their message; URLs are shared. The NUL
// separators keep a crafted src from colliding with another message's cid key.
func imgKey(msgID int, src string) string {
	if strings.HasPrefix(strings.ToLower(src), "cid:") {
		return fmt.Sprintf("cid\x00%d\x00%s", msgID, src)
	}
	return "url\x00" + src
}

// lookup returns a cached image with its placement token, or nil and whether
// it still needs fetching.
func (s *imageStore) lookup(msgID int, src string) (image.Image, bool, string) {
	if s == nil {
		return nil, false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := imgKey(msgID, src)
	if s.seen[msgID] == nil {
		s.seen[msgID] = map[string]bool{}
	}
	if !s.seen[msgID][k] {
		if len(s.order[msgID]) >= maxImagesPerMsg {
			return nil, false, "" // shortcut: extra images stay as [alt]
		}
		s.seen[msgID][k] = true
		s.order[msgID] = append(s.order[msgID], k)
	}
	if img := s.imgs[k]; img != nil {
		if s.noSixel[k] {
			return img, false, "" // no token: rendered with half-blocks
		}
		t := hash(k)
		s.keyOf[t] = k
		return img, false, t
	}
	if s.state[k] != 0 {
		return nil, false, ""
	}
	s.state[k] = imgPending
	return nil, true, ""
}

// sixelReq is the on-screen part of an inline image: cell rows [top, top+n).
type sixelReq struct {
	p      render.Placement
	top, n int
	row    int // screen row (within the preview) of the image's last visible row
}

func sixelKey(r sixelReq, cw, ch int) string {
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d", r.p.Token, r.p.Cols, r.p.Rows, cw, ch, r.top, r.n)
}

// sixelFor returns the encoded sixel for a request if it is ready.
func (s *imageStore) sixelFor(r sixelReq, cw, ch int) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sixel[sixelKey(r, cw, ch)]
}

type (
	sixelMsg  struct{ failed bool } // failed: some image fell back to blocks, re-render
	sixelTick struct{ offset int }  // scrolling paused here: encode what is visible
)

// encodeSixels encodes the requests that have no cached sixel yet.
func encodeSixels(s *imageStore, reqs []sixelReq, cw, ch int) tea.Cmd {
	if s == nil || len(reqs) == 0 {
		return nil
	}
	type job struct {
		key, path string
		r         sixelReq
	}
	var jobs []job
	s.mu.Lock()
	if len(s.sixel) > 64 { // shortcut: crude bound on encoded-image memory; LRU if it matters
		s.sixel = map[string]string{}
	}
	for _, r := range reqs {
		k := sixelKey(r, cw, ch)
		if _, ok := s.sixel[k]; ok {
			continue
		}
		if path := s.paths[s.keyOf[r.p.Token]]; path != "" {
			s.sixel[k] = "" // in progress
			jobs = append(jobs, job{k, path, r})
		}
	}
	s.mu.Unlock()
	if len(jobs) == 0 {
		return nil
	}
	return func() tea.Msg {
		failed := false
		for _, j := range jobs {
			enc := ""
			if img, err := decode(j.path, j.r.p.Cols*cw); err == nil { // full resolution, not the thumbnail
				enc = termimg.SixelCells(img, j.r.p.Cols, j.r.p.Rows, cw, ch, j.r.top, j.r.n)
			}
			s.mu.Lock()
			if enc == "" {
				delete(s.sixel, j.key)
				s.noSixel[s.keyOf[j.r.p.Token]] = true
				failed = true
			} else {
				s.sixel[j.key] = enc
			}
			s.mu.Unlock()
		}
		return sixelMsg{failed}
	}
}

// files are the original image files of a message, in document order.
func (s *imageStore) files(msgID int) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.order[msgID] {
		if p := s.paths[k]; p != "" && s.imgs[k] != nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *imageStore) put(k, path string, img image.Image, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state[k] = imgFailed
		os.Remove(path)
		return
	}
	// shortcut: unbounded for the session (images are shrunk, ~1MB each worst case); add LRU if memory grows.
	s.imgs[k], s.paths[k] = img, path
}

type imagesMsg struct{ msgID int }

// httpClient refuses loopback/private/link-local targets (also after
// redirects), so an email can't make us poke local services such as tb-bridge.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

func publicOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("refusing to fetch image from %s", host)
	}
	return nil
}

// fetchImages downloads srcs of a message concurrently, then reports back.
func fetchImages(ctx context.Context, c *tb.Client, s *imageStore, msgID int, srcs []string) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		var parts map[string]string // content id -> part name, loaded once if needed
		var partsOnce sync.Once
		sem := make(chan struct{}, 6)
		var wg sync.WaitGroup
		for _, src := range srcs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				k := imgKey(msgID, src)
				path := filepath.Join(s.dir, hash(k))
				err := func() error {
					switch u := strings.ToLower(src); {
					case strings.HasPrefix(u, "cid:"):
						partsOnce.Do(func() { parts = cidParts(ctx, c, msgID) })
						part, ok := parts[strings.Trim(src[4:], "<>")]
						if !ok {
							return fmt.Errorf("no part for %s", src)
						}
						return c.Download(ctx, msgID, part, path)
					case strings.HasPrefix(u, "data:"):
						return saveDataURI(src, path)
					case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
						return download(ctx, src, path)
					}
					return fmt.Errorf("unsupported image source")
				}()
				var img image.Image
				if err == nil {
					img, err = decode(path, cacheWidth)
				}
				s.put(k, path, img, err)
			}()
		}
		wg.Wait()
		return imagesMsg{msgID}
	}
}

func cidParts(ctx context.Context, c *tb.Client, msgID int) map[string]string {
	m := map[string]string{}
	as, _ := c.Attachments(ctx, msgID)
	for _, a := range as {
		if a.ContentID != "" {
			m[strings.Trim(a.ContentID, "<>")] = a.PartName
		}
	}
	return m
}

func download(ctx context.Context, src, path string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) lazythunder")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", resp.Status)
	}
	return save(path, io.LimitReader(resp.Body, maxImageBytes+1))
}

func saveDataURI(src, path string) error {
	meta, data, ok := strings.Cut(src, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") || len(data) > maxImageBytes*4/3 {
		return fmt.Errorf("unsupported data URI")
	}
	if d, err := url.PathUnescape(data); err == nil {
		data = d
	}
	return save(path, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
}

func save(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxImageBytes {
		err = fmt.Errorf("image larger than %d bytes", maxImageBytes)
	}
	return err
}

// decode reads an image file (guarding against decompression bombs) and
// shrinks it to at most maxW pixels wide.
func decode(path string, maxW int) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > maxImageBytes {
		return nil, fmt.Errorf("image too large")
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if float64(cfg.Width)*float64(cfg.Height) > maxImagePixels {
		return nil, fmt.Errorf("image dimensions too large (%dx%d)", cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return termimg.Shrink(img, maxW), nil
}

func hash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:8])
}

// viewerCmd runs the full-screen image viewer in-process while bubbletea
// has released the terminal (tea.Exec).
type viewerCmd struct{ files []string }

func (v viewerCmd) Run() error        { return termimg.View(v.files) }
func (viewerCmd) SetStdin(io.Reader)  {}
func (viewerCmd) SetStdout(io.Writer) {}
func (viewerCmd) SetStderr(io.Writer) {}
