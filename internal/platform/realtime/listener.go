package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Channel is the PostgreSQL NOTIFY channel raised by the realtime triggers.
const Channel = "previa_events"

// Connector opens a dedicated connection. LISTEN state belongs to a session, so
// the listener never borrows a pooled connection.
type Connector func(context.Context) (*pgx.Conn, error)

// Listen forwards NOTIFY payloads to hub until ctx ends, reconnecting with
// capped backoff. Each successful LISTEN asks the hub to resync, because
// notifications raised while no session was listening are lost.
func Listen(ctx context.Context, connect Connector, hub *Hub, logger *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, connect, hub, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		logger.WarnContext(ctx, "realtime listener disconnected; retrying", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func listenOnce(ctx context.Context, connect Connector, hub *Hub, connected func()) error {
	conn, err := connect(ctx)
	if err != nil {
		return fmt.Errorf("connect realtime listener: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return fmt.Errorf("listen for realtime events: %w", err)
	}
	connected()
	hub.Resync()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for realtime event: %w", err)
		}
		topic, source, err := decode(n.Payload)
		if err != nil {
			continue // Foreign or malformed payloads on the channel are ignored.
		}
		hub.Publish(topic, source)
	}
}

func decode(payload string) (topic, source string, err error) {
	var message struct {
		Topic  string `json:"topic"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		return "", "", fmt.Errorf("decode realtime payload: %w", err)
	}
	if message.Topic == "" {
		return "", "", errors.New("realtime payload has no topic")
	}
	return message.Topic, message.Source, nil
}
