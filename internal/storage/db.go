package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DB wraps the SQLite database with app-specific operations.
type DB struct {
	conn *sql.DB
}

// New creates a new database connection and runs migrations.
func New(dbPath string) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	conn, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			phone TEXT PRIMARY KEY,
			authenticated INTEGER NOT NULL DEFAULT 0,
			session_token TEXT,
			last_active DATETIME,
			failed_attempts INTEGER NOT NULL DEFAULT 0,
			locked_until DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_phone ON messages(phone, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			args TEXT,
			result_summary TEXT,
			success INTEGER NOT NULL,
			duration_ms INTEGER,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS pending_confirmations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			args TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(phone)
		)`,
	}

	for _, m := range migrations {
		if _, err := db.conn.Exec(m); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, m)
		}
	}
	return nil
}

// --- Auth Sessions ---

// GetSession retrieves the auth session for a phone number.
func (db *DB) GetSession(phone string) (*AuthSession, error) {
	row := db.conn.QueryRow(
		"SELECT phone, authenticated, session_token, last_active, failed_attempts, locked_until FROM auth_sessions WHERE phone = ?",
		phone,
	)

	s := &AuthSession{}
	var lastActive, lockedUntil sql.NullTime
	var token sql.NullString
	err := row.Scan(&s.Phone, &s.Authenticated, &token, &lastActive, &s.FailedAttempts, &lockedUntil)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SessionToken = token.String
	s.LastActive = lastActive.Time
	s.LockedUntil = lockedUntil.Time
	return s, nil
}

// UpsertSession creates or updates an auth session.
func (db *DB) UpsertSession(s *AuthSession) error {
	_, err := db.conn.Exec(`
		INSERT INTO auth_sessions (phone, authenticated, session_token, last_active, failed_attempts, locked_until)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(phone) DO UPDATE SET
			authenticated = excluded.authenticated,
			session_token = excluded.session_token,
			last_active = excluded.last_active,
			failed_attempts = excluded.failed_attempts,
			locked_until = excluded.locked_until
	`, s.Phone, s.Authenticated, s.SessionToken, s.LastActive, s.FailedAttempts, s.LockedUntil)
	return err
}

// AuthSession represents a user's authentication state.
type AuthSession struct {
	Phone          string
	Authenticated  bool
	SessionToken   string
	LastActive     time.Time
	FailedAttempts int
	LockedUntil    time.Time
}

// --- Messages ---

// SaveMessage stores a message in conversation history.
func (db *DB) SaveMessage(phone, role, content string) error {
	_, err := db.conn.Exec(
		"INSERT INTO messages (phone, role, content) VALUES (?, ?, ?)",
		phone, role, content,
	)
	return err
}

// GetMessages retrieves the last N messages for a phone number.
func (db *DB) GetMessages(phone string, limit int) ([]Message, error) {
	rows, err := db.conn.Query(
		"SELECT role, content, created_at FROM messages WHERE phone = ? ORDER BY created_at DESC LIMIT ?",
		phone, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}

	// Reverse to get chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ClearMessages deletes all messages for a phone number.
func (db *DB) ClearMessages(phone string) error {
	_, err := db.conn.Exec("DELETE FROM messages WHERE phone = ?", phone)
	return err
}

// Message represents a single chat message.
type Message struct {
	Role      string
	Content   string
	CreatedAt time.Time
}

// --- Audit Log ---

// LogAudit records a tool invocation to the audit log.
func (db *DB) LogAudit(phone, toolName, args, resultSummary string, success bool, durationMs int64) error {
	_, err := db.conn.Exec(
		"INSERT INTO audit_log (phone, tool_name, args, result_summary, success, duration_ms) VALUES (?, ?, ?, ?, ?, ?)",
		phone, toolName, args, resultSummary, success, durationMs,
	)
	return err
}

// --- Pending Confirmations ---

// SetPendingConfirmation stores a pending write confirmation.
func (db *DB) SetPendingConfirmation(phone, toolName, args string) error {
	_, err := db.conn.Exec(`
		INSERT INTO pending_confirmations (phone, tool_name, args)
		VALUES (?, ?, ?)
		ON CONFLICT(phone) DO UPDATE SET
			tool_name = excluded.tool_name,
			args = excluded.args,
			created_at = CURRENT_TIMESTAMP
	`, phone, toolName, args)
	return err
}

// GetPendingConfirmation retrieves and deletes a pending confirmation.
func (db *DB) GetPendingConfirmation(phone string) (toolName, args string, err error) {
	row := db.conn.QueryRow(
		"SELECT tool_name, args FROM pending_confirmations WHERE phone = ?", phone,
	)
	err = row.Scan(&toolName, &args)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	// Delete after reading
	db.conn.Exec("DELETE FROM pending_confirmations WHERE phone = ?", phone)
	return toolName, args, nil
}

// DeletePendingConfirmation removes a pending confirmation without executing.
func (db *DB) DeletePendingConfirmation(phone string) error {
	_, err := db.conn.Exec("DELETE FROM pending_confirmations WHERE phone = ?", phone)
	return err
}
