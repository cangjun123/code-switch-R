package services

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestEnsureRequestLogTableMigratesLegacyColumns(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()

	legacySchema := `CREATE TABLE request_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		platform TEXT,
		model TEXT,
		provider TEXT,
		http_code INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		is_stream INTEGER DEFAULT 0,
		duration_sec REAL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy request_log: %v", err)
	}

	if err := ensureRequestLogTableWithDB(db); err != nil {
		t.Fatalf("ensureRequestLogTableWithDB: %v", err)
	}

	rows, err := db.Query(`PRAGMA table_info(request_log)`)
	if err != nil {
		t.Fatalf("pragma table_info(request_log): %v", err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma rows: %v", err)
	}

	for _, column := range []string{"cache_create_tokens", "cache_read_tokens", "reasoning_tokens"} {
		if !columns[column] {
			t.Fatalf("expected migrated column %q to exist", column)
		}
	}
}

func TestIsClientAbortError(t *testing.T) {
	t.Parallel()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{
			name: "wrapped context canceled",
			ctx:  context.Background(),
			err:  errors.New("context canceled"),
			want: true,
		},
		{
			name: "context already canceled",
			ctx:  canceledCtx,
			err:  errors.New("request failed"),
			want: true,
		},
		{
			name: "broken pipe",
			ctx:  context.Background(),
			err:  errors.New("write tcp 127.0.0.1: broken pipe"),
			want: true,
		},
		{
			name: "dns timeout",
			ctx:  context.Background(),
			err:  errors.New("lookup yuzapi.fun on 127.0.0.53:53: read udp 127.0.0.1:53711->127.0.0.53:53: i/o timeout"),
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isClientAbortError(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("isClientAbortError() = %v, want %v", got, tt.want)
			}
		})
	}
}
