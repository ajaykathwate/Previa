// Command reference is the reference journeys' fixture (docs/31-test-data.md,
// docs/32-persistent-test-data.md): two small apps a repository would build
// from its own Dockerfile.
//
//	reference shop serve    an HTTP API with SQL data (synthetic orders)
//	reference shop setup    create the schema, seed two orders (fails when SEED_FAIL=1)
//	reference shop migrate  create or migrate the schema only, on any data:
//	                        MIGRATION=2 adds a column to existing rows,
//	                        REJECT_EXISTING=1 refuses a database that already
//	                        has rows (a newer migration history), MIGRATE_FAIL=1
//	                        always fails (a broken migration)
//	reference shop seed     insert the two seed orders; not repeatable on
//	                        purpose, so it fails on a database that has them
//	reference notes serve   a web app with a test login
//	reference notes setup   create the schema, seed three notes and the
//	                        preview-only account reviewer@preview.test whose
//	                        password is the TEST_LOGIN_PASSWORD secret
//
// The setup seeds are repeatable (they insert only what is missing); all
// data is synthetic.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// testAccount is the preview-only account the notes seed creates.
const testAccount = "reviewer@preview.test"

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: reference shop|notes serve|setup")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] + " " + os.Args[2] {
	case "shop serve":
		err = serveShop()
	case "shop setup":
		err = setupShop()
	case "shop migrate":
		err = migrateShop()
	case "shop seed":
		err = seedShop()
	case "notes serve":
		err = serveNotes()
	case "notes setup":
		err = setupNotes()
	default:
		err = errors.New("unknown command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reference:", err)
		os.Exit(1)
	}
}

func database(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
}

func run(ctx context.Context, conn *pgx.Conn, statements ...string) error {
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func health(w http.ResponseWriter, r *http.Request) {
	conn, err := database(r.Context())
	if err == nil {
		err = conn.Ping(r.Context())
		_ = conn.Close(r.Context())
	}
	if err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

func listen(mux *http.ServeMux) error {
	mux.HandleFunc("/health", health)
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

// --- The shop API -----------------------------------------------------------

type order struct {
	ID         int    `json:"id"`
	Customer   string `json:"customer"`
	TotalCents int    `json:"total_cents"`
	Source     string `json:"source"`
}

func setupShop() error {
	ctx := context.Background()
	if os.Getenv("SEED_FAIL") == "1" {
		fmt.Println("seeding: orders.sql")
		return errors.New("seed orders.sql failed: relation \"customers\" does not exist")
	}
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := run(ctx, conn,
		`CREATE TABLE IF NOT EXISTS orders (id SERIAL PRIMARY KEY, customer TEXT NOT NULL UNIQUE, total_cents INTEGER NOT NULL, source TEXT NOT NULL)`,
		`INSERT INTO orders (customer, total_cents, source) VALUES ('Preview customer A', 1250, 'seed'), ('Preview customer B', 4999, 'seed')
		 ON CONFLICT (customer) DO NOTHING`,
	); err != nil {
		return err
	}
	fmt.Println("shop: migrated and seeded 2 orders")
	return nil
}

// migrateShop brings the schema up to date on whatever data the database
// starts with, and says how much was already there.
func migrateShop() error {
	ctx := context.Background()
	if os.Getenv("MIGRATE_FAIL") == "1" {
		fmt.Println("migrating: 0002_discounts.sql")
		return errors.New("migration 0002_discounts.sql failed: syntax error at or near \"DISCOUNT\"")
	}
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := run(ctx, conn, `CREATE TABLE IF NOT EXISTS orders (id SERIAL PRIMARY KEY, customer TEXT NOT NULL UNIQUE, total_cents INTEGER NOT NULL, source TEXT NOT NULL)`); err != nil {
		return err
	}
	var existing int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&existing); err != nil {
		return err
	}
	if existing > 0 && os.Getenv("REJECT_EXISTING") == "1" {
		return fmt.Errorf("the database's migration history is newer than this branch (%d orders); refusing to migrate", existing)
	}
	if os.Getenv("MIGRATION") == "2" {
		if err := run(ctx, conn, `ALTER TABLE orders ADD COLUMN IF NOT EXISTS discount_code TEXT`); err != nil {
			return err
		}
	}
	fmt.Printf("shop: migrated; %d orders were already there\n", existing)
	return nil
}

// seedShop inserts the seed orders. It is not repeatable on purpose: Previa
// runs a seed only on an empty database.
func seedShop() error {
	ctx := context.Background()
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := run(ctx, conn, `INSERT INTO orders (customer, total_cents, source) VALUES ('Preview customer A', 1250, 'seed'), ('Preview customer B', 4999, 'seed')`); err != nil {
		return err
	}
	fmt.Println("shop: seeded 2 orders")
	return nil
}

func serveShop() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = conn.Close(r.Context()) }()
		if r.Method == http.MethodPost {
			customer := strings.TrimSpace(r.FormValue("customer"))
			if customer == "" {
				http.Error(w, "customer is required", http.StatusBadRequest)
				return
			}
			if _, err := conn.Exec(r.Context(), "INSERT INTO orders (customer, total_cents, source) VALUES ($1, 100, 'tester')", customer); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		rows, err := conn.Query(r.Context(), "SELECT id, customer, total_cents, source FROM orders ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		orders, err := pgx.CollectRows(rows, pgx.RowToStructByPos[order])
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(orders)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "Shop API\n\nGET /orders lists orders (two are seeded: Preview customer A and B).\nPOST /orders with customer=<name> adds one.\nGET /columns lists the orders table's columns.\n")
	})
	mux.HandleFunc("/columns", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = conn.Close(r.Context()) }()
		rows, err := conn.Query(r.Context(), "SELECT column_name FROM information_schema.columns WHERE table_name = 'orders' ORDER BY ordinal_position")
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(columns)
	})
	return listen(mux)
}

// --- The notes web app with a test login -------------------------------------

func passwordHash(password string) string {
	sum := sha256.Sum256([]byte("reference-fixture:" + password))
	return hex.EncodeToString(sum[:])
}

func setupNotes() error {
	ctx := context.Background()
	conn, err := database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := run(ctx, conn,
		`CREATE TABLE IF NOT EXISTS accounts (email TEXT PRIMARY KEY, password_hash TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS notes (id SERIAL PRIMARY KEY, body TEXT NOT NULL UNIQUE, author TEXT NOT NULL)`,
		`INSERT INTO notes (body, author) VALUES ('Welcome to the preview', 'seed'), ('Try adding a note', 'seed'), ('Notes reset on every new deployment', 'seed')
		 ON CONFLICT (body) DO NOTHING`,
	); err != nil {
		return err
	}
	password := os.Getenv("TEST_LOGIN_PASSWORD")
	if password == "" {
		// A fork's preview gets no repository secrets: say exactly what is missing.
		fmt.Println("notes: seeded 3 notes; TEST_LOGIN_PASSWORD is not set, so this preview has no test login")
		return nil
	}
	if _, err := conn.Exec(ctx, `INSERT INTO accounts (email, password_hash) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash`, testAccount, passwordHash(password)); err != nil {
		return err
	}
	fmt.Println("notes: seeded 3 notes and the preview-only account " + testAccount)
	return nil
}

func serveNotes() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	sign := func(email string) string {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(email))
		return email + "|" + hex.EncodeToString(mac.Sum(nil))
	}
	signedIn := func(r *http.Request) string {
		c, err := r.Cookie("session")
		if err != nil {
			return ""
		}
		email, _, _ := strings.Cut(c.Value, "|")
		if subtle.ConstantTimeCompare([]byte(sign(email)), []byte(c.Value)) != 1 {
			return ""
		}
		return email
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		conn, err := database(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = conn.Close(r.Context()) }()
		var accounts int
		_ = conn.QueryRow(r.Context(), "SELECT count(*) FROM accounts").Scan(&accounts)
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if accounts == 0 {
				_, _ = io.WriteString(w, "<p>Test login unavailable in this preview: TEST_LOGIN_PASSWORD was not provided (forks get no secrets).</p>")
				return
			}
			_, _ = io.WriteString(w, `<form method="post"><input name="email"><input name="password" type="password"><button>Sign in</button></form>`)
			return
		}
		var stored string
		err = conn.QueryRow(r.Context(), "SELECT password_hash FROM accounts WHERE email = $1", r.FormValue("email")).Scan(&stored)
		if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(passwordHash(r.FormValue("password")))) != 1 {
			http.Error(w, "wrong email or password", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: sign(r.FormValue("email")), Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
		email := signedIn(r)
		if email == "" || r.Method != http.MethodPost {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		conn, err := database(r.Context())
		if err == nil {
			_, err = conn.Exec(r.Context(), "INSERT INTO notes (body, author) VALUES ($1, $2)", r.FormValue("body"), email)
			_ = conn.Close(r.Context())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		email := signedIn(r)
		if email == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		conn, err := database(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = conn.Close(r.Context()) }()
		rows, err := conn.Query(r.Context(), "SELECT body FROM notes ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		notes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<p>Signed in as %s</p><ul>", html.EscapeString(email))
		for _, n := range notes {
			_, _ = fmt.Fprintf(w, "<li>%s</li>", html.EscapeString(n))
		}
		_, _ = io.WriteString(w, "</ul>")
	})
	return listen(mux)
}
