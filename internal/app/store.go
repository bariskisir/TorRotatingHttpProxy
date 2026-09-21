package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

type IPRecord struct {
	IP   string `json:"ip"`
	Used bool   `json:"used"`
}

type IPPage struct {
	Items    []IPRecord `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

type Store struct {
	db *sql.DB
	// Serializes short transactions and keeps counts consistent with the page.
	mu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS ips (
		ip TEXT PRIMARY KEY,
		used INTEGER NOT NULL DEFAULT 0 CHECK (used IN (0, 1))
	); CREATE INDEX IF NOT EXISTS ips_used_ip ON ips(used, ip);`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Observe(ctx context.Context, ip string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO ips(ip, used) VALUES(?, 0) ON CONFLICT(ip) DO NOTHING", ip); err != nil {
		return false, err
	}
	var used bool
	err := s.db.QueryRowContext(ctx, "SELECT used FROM ips WHERE ip=?", ip).Scan(&used)
	return !used, err
}

// Consume commits before any user traffic is sent. A failed request still consumes
// its address: delivery cannot reliably be inferred after a connection failure.
func (s *Store) Consume(ctx context.Context, ip string, unique bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := "UPDATE ips SET used=1 WHERE ip=?"
	if unique {
		query += " AND used=0"
	}
	result, err := s.db.ExecContext(ctx, query, ip)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) Counts(ctx context.Context) (total, used int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(used), 0) FROM ips").Scan(&total, &used)
	return
}

func (s *Store) List(ctx context.Context, search, filter string, page, size int) (IPPage, error) {
	p := IPPage{Items: []IPRecord{}, Page: page, PageSize: size}
	if page < 1 || size < 1 || size > 100 {
		return p, fmt.Errorf("invalid pagination")
	}
	where := " WHERE instr(ip, ?) > 0"
	args := []any{search}
	if filter != "" {
		if filter != "true" && filter != "false" {
			return p, fmt.Errorf("used must be true or false")
		}
		where += " AND used=?"
		args = append(args, filter == "true")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ips"+where, args...).Scan(&p.Total); err != nil {
		return p, err
	}
	args = append(args, size, int64(page-1)*int64(size))
	rows, err := s.db.QueryContext(ctx, "SELECT ip, used FROM ips"+where+" ORDER BY ip LIMIT ? OFFSET ?", args...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var item IPRecord
		if err := rows.Scan(&item.IP, &item.Used); err != nil {
			return p, err
		}
		p.Items = append(p.Items, item)
	}
	return p, rows.Err()
}
