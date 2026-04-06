package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/config"
	"github.com/USA-graphene/Antigravity-WhatsApp-Connector/internal/storage"
)

// Engine handles authentication and session management.
type Engine struct {
	cfg *config.AuthConfig
	db  *storage.DB
	log *slog.Logger
}

// NewEngine creates a new auth engine.
func NewEngine(cfg *config.AuthConfig, db *storage.DB, log *slog.Logger) *Engine {
	return &Engine{cfg: cfg, db: db, log: log}
}

// Result represents an authentication check result.
type Result struct {
	Allowed bool
	Message string // Message to send back to user (if not allowed)
}

// CheckAccess verifies if a phone number is authorized and authenticated.
// Returns (allowed, message_for_user).
func (e *Engine) CheckAccess(phone string) Result {
	// Step 1: Check phone allowlist
	if !e.isPhoneAllowed(phone) {
		e.log.Warn("rejected unauthorized phone", "phone", phone)
		return Result{Allowed: false, Message: ""}
	}

	// Step 2: Check/create session
	session, err := e.db.GetSession(phone)
	if err != nil {
		e.log.Error("failed to get session", "phone", phone, "error", err)
		return Result{Allowed: false, Message: "⚠️ Internal error. Try again later."}
	}

	// New user - create session and require PIN
	if session == nil {
		e.db.UpsertSession(&storage.AuthSession{
			Phone:         phone,
			Authenticated: false,
		})
		if e.cfg.PIN == "" {
			// No PIN configured - auto-authenticate
			e.authenticate(phone)
			return Result{Allowed: true}
		}
		return Result{
			Allowed: false,
			Message: "🔐 *Authentication Required*\n\nPlease enter your PIN to continue:",
		}
	}

	// Check lockout
	if !session.LockedUntil.IsZero() && time.Now().Before(session.LockedUntil) {
		remaining := time.Until(session.LockedUntil).Round(time.Minute)
		return Result{
			Allowed: false,
			Message: fmt.Sprintf("🚫 Account locked. Try again in %v.", remaining),
		}
	}

	// Check if session is still valid
	if session.Authenticated {
		ttl := e.cfg.GetSessionTTL()
		if time.Since(session.LastActive) > ttl {
			// Session expired
			e.log.Info("session expired", "phone", phone)
			session.Authenticated = false
			e.db.UpsertSession(session)
			if e.cfg.PIN == "" {
				e.authenticate(phone)
				return Result{Allowed: true}
			}
			return Result{
				Allowed: false,
				Message: "🔐 *Session Expired*\n\nPlease enter your PIN to continue:",
			}
		}
		// Update last active
		session.LastActive = time.Now()
		e.db.UpsertSession(session)
		return Result{Allowed: true}
	}

	// Not authenticated - needs PIN
	if e.cfg.PIN == "" {
		e.authenticate(phone)
		return Result{Allowed: true}
	}
	return Result{
		Allowed: false,
		Message: "🔐 Please enter your PIN:",
	}
}

// TryPIN attempts to authenticate with a PIN.
func (e *Engine) TryPIN(phone, pin string) Result {
	if !e.isPhoneAllowed(phone) {
		return Result{Allowed: false, Message: ""}
	}

	session, err := e.db.GetSession(phone)
	if err != nil || session == nil {
		return Result{Allowed: false, Message: "⚠️ Internal error."}
	}

	// Check lockout
	if !session.LockedUntil.IsZero() && time.Now().Before(session.LockedUntil) {
		remaining := time.Until(session.LockedUntil).Round(time.Minute)
		return Result{
			Allowed: false,
			Message: fmt.Sprintf("🚫 Account locked. Try again in %v.", remaining),
		}
	}

	if strings.TrimSpace(pin) == e.cfg.PIN {
		e.authenticate(phone)
		e.log.Info("PIN authentication successful", "phone", phone)
		return Result{
			Allowed: true,
			Message: "✅ *Authenticated!*\n\nYou're connected to Antigravity. How can I help?",
		}
	}

	// Wrong PIN
	session.FailedAttempts++
	if session.FailedAttempts >= e.cfg.MaxFailedAttempts {
		session.LockedUntil = time.Now().Add(15 * time.Minute)
		e.db.UpsertSession(session)
		e.log.Warn("account locked after too many attempts", "phone", phone)
		return Result{
			Allowed: false,
			Message: "🚫 Too many failed attempts. Account locked for 15 minutes.",
		}
	}

	e.db.UpsertSession(session)
	remaining := e.cfg.MaxFailedAttempts - session.FailedAttempts
	e.log.Warn("wrong PIN", "phone", phone, "attempts_remaining", remaining)
	return Result{
		Allowed: false,
		Message: fmt.Sprintf("❌ Wrong PIN. %d attempts remaining.", remaining),
	}
}

// IsAuthenticated checks if a phone number is currently authenticated.
func (e *Engine) IsAuthenticated(phone string) bool {
	session, err := e.db.GetSession(phone)
	if err != nil || session == nil {
		return false
	}
	if !session.Authenticated {
		return false
	}
	ttl := e.cfg.GetSessionTTL()
	return time.Since(session.LastActive) <= ttl
}

// Logout ends a session.
func (e *Engine) Logout(phone string) {
	e.db.UpsertSession(&storage.AuthSession{
		Phone:         phone,
		Authenticated: false,
	})
	e.db.ClearMessages(phone)
	e.log.Info("user logged out", "phone", phone)
}

func (e *Engine) authenticate(phone string) {
	token := generateToken()
	e.db.UpsertSession(&storage.AuthSession{
		Phone:         phone,
		Authenticated: true,
		SessionToken:  token,
		LastActive:    time.Now(),
	})
}

func (e *Engine) isPhoneAllowed(phone string) bool {
	if len(e.cfg.AllowedPhones) == 0 {
		return false // No phones configured = no access
	}
	for _, p := range e.cfg.AllowedPhones {
		if p == phone {
			return true
		}
	}
	return false
}

func generateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
