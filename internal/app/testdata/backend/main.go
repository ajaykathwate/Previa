// Command backend is the e2e test's backend fixture: an HTTP API on
// PostgreSQL and Redis with a migration-and-seed setup command and a queue
// worker, as a repository would declare them in .previewrc.yaml.
//
//	backend serve    the API on :8080
//	backend migrate  create the schema and seed it (fails when MIGRATION_FAIL=1)
//	backend worker   move jobs from Redis into PostgreSQL
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: backend serve|migrate|worker")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "migrate":
		err = migrate()
	case "worker":
		err = worker()
	default:
		err = errors.New("unknown command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "backend:", err)
		os.Exit(1)
	}
}

func database(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
}

func migrate() error {
	ctx := context.Background()
	if os.Getenv("MIGRATION_FAIL") == "1" {
		fmt.Println("migrating: applying 0002_broken.sql")
		return errors.New("migration 0002_broken.sql failed: syntax error at or near \"BROKEN\"")
	}
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	for _, statement := range []string{
		"CREATE TABLE identity (id TEXT NOT NULL)",
		"CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT NOT NULL)",
		"CREATE TABLE processed (job TEXT PRIMARY KEY)",
		"INSERT INTO items (name) VALUES ('seed-one'), ('seed-two')",
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO identity VALUES ($1)", hex.EncodeToString(raw)); err != nil {
		return err
	}
	fmt.Println("migrated and seeded 2 items")
	return nil
}

func worker() error {
	ctx := context.Background()
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	redis, err := dialRedis()
	if err != nil {
		return err
	}
	fmt.Println("worker waiting for jobs")
	for {
		reply, err := redis.do("BLPOP", "jobs", "5")
		if err != nil {
			return err
		}
		if len(reply) == 2 {
			if _, err := conn.Exec(ctx, "INSERT INTO processed VALUES ($1) ON CONFLICT DO NOTHING", reply[1]); err != nil {
				return err
			}
		}
	}
}

func serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err == nil {
			err = conn.Ping(r.Context())
			_ = conn.Close(r.Context())
		}
		if err == nil {
			var redis *redisConn
			if redis, err = dialRedis(); err == nil {
				_, err = redis.do("PING")
				redis.close()
			}
		}
		if err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = conn.Close(r.Context()) }()
		var state struct {
			Marker    string   `json:"marker"`
			Identity  string   `json:"identity"`
			Items     int      `json:"items"`
			Processed []string `json:"processed"`
		}
		state.Marker = os.Getenv("MARKER")
		_ = conn.QueryRow(r.Context(), "SELECT id FROM identity").Scan(&state.Identity)
		_ = conn.QueryRow(r.Context(), "SELECT count(*) FROM items").Scan(&state.Items)
		rows, err := conn.Query(r.Context(), "SELECT job FROM processed ORDER BY job")
		if err == nil {
			for rows.Next() {
				var job string
				_ = rows.Scan(&job)
				state.Processed = append(state.Processed, job)
			}
			rows.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err == nil {
			_, err = conn.Exec(r.Context(), "INSERT INTO items (name) VALUES ('added')")
			_ = conn.Close(r.Context())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		redis, err := dialRedis()
		if err == nil {
			_, err = redis.do("RPUSH", "jobs", r.URL.Query().Get("id"))
			redis.close()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "backend fixture API docs") })
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

// redisConn is a minimal RESP client: enough for PING, RPUSH and BLPOP.
type redisConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialRedis() (*redisConn, error) {
	u, err := url.Parse(os.Getenv("REDIS_URL"))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &redisConn{conn: conn, r: bufio.NewReader(conn)}
	if password, ok := u.User.Password(); ok {
		if _, err := c.do("AUTH", password); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

func (c *redisConn) close() { _ = c.conn.Close() }

func (c *redisConn) do(args ...string) ([]string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, err
	}
	return c.reply()
}

func (c *redisConn) reply() ([]string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	switch line[0] {
	case '+', ':':
		return []string{line[1:]}, nil
	case '-':
		return nil, errors.New(line[1:])
	case '$':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return []string{string(buf[:n])}, nil
	case '*':
		n, _ := strconv.Atoi(line[1:])
		var out []string
		for range max(n, 0) {
			item, err := c.reply()
			if err != nil {
				return nil, err
			}
			out = append(out, item...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}
