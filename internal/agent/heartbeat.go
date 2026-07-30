package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Beacon reports this agent's liveness to its primary on an interval.
//
// The agent pings OUT rather than the primary polling in: the agent is the side
// that knows it is alive, may sit behind NAT, and may change networks. Requiring
// inbound reachability for liveness would make a laptop look permanently down
// even while it is happily serving.
//
// Failure to reach the primary is logged and retried, never fatal. An agent
// whose primary is restarting should keep supervising its backends and reconnect,
// not exit and strand them.
type Beacon struct {
	Primary  string // base URL of the primary, e.g. http://box1:8111
	Server   string // the `servers:` key this agent backs
	Token    string // this server's agent token
	Interval time.Duration
	Srv      *Server
	// SelfUpdate lets the agent replace its own binary with the primary's build
	// when the versions differ AND nothing is running here. On by default: a
	// fleet of agents pinned to old builds by hand is the failure mode this
	// exists to avoid, and the idle condition keeps it from costing anyone a
	// running backend. Set selfUpdate:false (or --self-update=false) to pin.
	SelfUpdate bool
	cli        *http.Client
}

// Run beats until ctx ends. Blocking; call it in a goroutine.
func (b *Beacon) Run(ctx context.Context) {
	if b.Primary == "" || b.Server == "" {
		slog.Info("agent: no primary configured; not sending heartbeats")
		return
	}
	if b.Interval <= 0 {
		b.Interval = HeartbeatInterval
	}
	if b.cli == nil {
		// Short: a heartbeat that has not landed within a fraction of its own
		// interval has already been overtaken by the next one.
		b.cli = &http.Client{Timeout: 5 * time.Second}
	}
	slog.Info("agent: heartbeating", "primary", b.Primary, "server", b.Server, "every", b.Interval)

	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		ack, err := b.beat(ctx)
		if err != nil {
			slog.Warn("agent: heartbeat failed", "primary", b.Primary, "err", err)
		} else {
			b.maybeSelfUpdate(ctx, ack)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Beacon) beat(ctx context.Context) (HeartbeatAck, error) {
	var ack HeartbeatAck
	hb := Heartbeat{Server: b.Server}
	if b.Srv != nil {
		hb.Hello = b.Srv.hello()
		hb.Backends = b.Srv.snapshot()
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return ack, err
	}
	url := strings.TrimRight(b.Primary, "/") + "/api/v1/agents/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ack, err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	resp, err := b.cli.Do(req)
	if err != nil {
		return ack, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		// 401 is the operator revoking this agent's token — the deliberate way
		// to detach a machine. Say so plainly rather than as a generic failure,
		// because the fix is a config change, not a network one.
		if resp.StatusCode == http.StatusUnauthorized {
			return ack, fmt.Errorf("token rejected by the primary (revoked?): %s", strings.TrimSpace(string(msg)))
		}
		return ack, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ack)
	return ack, nil
}
