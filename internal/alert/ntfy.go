// Package alert pushes operator alerts to an ntfy topic: a provider refusing
// requests, a spend cap reached, a provider reporting its limit.
package alert

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Notifier posts to one ntfy topic URL (https://ntfy.sh/<topic>). A nil
// Notifier, or one with no URL, drops everything, so callers need no checks.
type Notifier struct {
	URL      string
	Client   *http.Client
	Throttle time.Duration // one push per key per this long; 0 = 30 min

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// New builds a notifier for url; "" gives one that drops everything.
func New(url string) *Notifier {
	return &Notifier{URL: strings.TrimSpace(url), Client: &http.Client{Timeout: 10 * time.Second}}
}

// Send pushes title + body unless key was pushed within the throttle window.
// It returns at once; the post runs in the background and a failure is only
// logged. tags are ntfy tags (emoji shortcodes such as "warning").
func (n *Notifier) Send(key, title, body string, tags ...string) {
	if n == nil || n.URL == "" || !n.allow(key) {
		return
	}
	go n.post(title, body, tags)
}

func (n *Notifier) allow(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if n.now != nil {
		now = n.now()
	}
	every := n.Throttle
	if every <= 0 {
		every = 30 * time.Minute
	}
	if t, ok := n.last[key]; ok && now.Sub(t) < every {
		return false
	}
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	n.last[key] = now
	return true
}

// Forget clears key's throttle, so the next Send for it goes out — used when a
// streak ends and the next one is news again.
func (n *Notifier) Forget(key string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.last, key)
	n.mu.Unlock()
}

func (n *Notifier) post(title, body string, tags []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
	if err != nil {
		slog.Warn("alert: bad ntfy url", "err", err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Markdown", "yes")
	if len(tags) > 0 {
		req.Header.Set("Tags", strings.Join(tags, ","))
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		slog.Warn("alert: ntfy push failed", "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("alert: ntfy push refused", "status", resp.StatusCode)
	}
}
