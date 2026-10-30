package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type logEntry struct {
	instance, line string
	at             time.Time
}

func (m *Manager) Enqueue(instance, line string, at time.Time) {
	m.mu.Lock()
	enabled := m.state.Token != ""
	m.mu.Unlock()
	if !enabled {
		return
	}
	if len(line) > 65536 {
		line = line[:65536]
	}
	select {
	case m.logs <- logEntry{instance, line, at}:
	default:
	}
}
func (m *Manager) RunLogs(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := []logEntry{}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-m.logs:
			batch = append(batch, e)
			if len(batch) >= 200 {
				m.pushLogs(ctx, batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				m.pushLogs(ctx, batch)
				batch = nil
			}
		}
	}
}
func (m *Manager) pushLogs(ctx context.Context, batch []logEntry) {
	m.mu.Lock()
	port := m.state.Config.LokiPort
	m.mu.Unlock()
	groups := map[string][][2]string{}
	for _, e := range batch {
		groups[e.instance] = append(groups[e.instance], [2]string{strconv.FormatInt(e.at.UnixNano(), 10), e.line})
	}
	streams := []any{}
	for id, values := range groups {
		streams = append(streams, map[string]any{"stream": map[string]string{"job": "craftstack", "instance": id}, "values": values})
	}
	b, _ := json.Marshal(map[string]any{"streams": streams})
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/loki/api/v1/push", port), bytes.NewReader(b))
		if e != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if e == nil {
			res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}
