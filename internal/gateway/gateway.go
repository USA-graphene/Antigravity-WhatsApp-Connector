package gateway

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// RateLimiter implements a simplified token bucket rate limiter.
type RateLimiter struct {
	perUser int
	global  int
	mu      sync.Mutex
	buckets map[string]*bucket
	gBucket *bucket
	log     *slog.Logger
}

type bucket struct {
	tokens    int
	maxTokens int
	lastRefill time.Time
	refillRate int // tokens per minute
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter(perUser, global int, log *slog.Logger) *RateLimiter {
	return &RateLimiter{
		perUser: perUser,
		global:  global,
		buckets: make(map[string]*bucket),
		gBucket: &bucket{
			tokens:    global,
			maxTokens: global,
			lastRefill: time.Now(),
			refillRate: global,
		},
		log: log,
	}
}

// Allow checks if a message from the given phone number should be allowed.
func (rl *RateLimiter) Allow(phone string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Refill and check global bucket
	rl.gBucket.refill()
	if rl.gBucket.tokens <= 0 {
		rl.log.Warn("global rate limit hit", "phone", phone)
		return false
	}

	// Refill and check per-user bucket
	b, ok := rl.buckets[phone]
	if !ok {
		b = &bucket{
			tokens:    rl.perUser,
			maxTokens: rl.perUser,
			lastRefill: time.Now(),
			refillRate: rl.perUser,
		}
		rl.buckets[phone] = b
	}

	b.refill()
	if b.tokens <= 0 {
		rl.log.Warn("per-user rate limit hit", "phone", phone)
		return false
	}

	// Consume tokens
	b.tokens--
	rl.gBucket.tokens--
	return true
}

func (b *bucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill)
	if elapsed < time.Second {
		return
	}

	// Add tokens proportional to time elapsed
	tokensToAdd := int(elapsed.Minutes() * float64(b.refillRate))
	if tokensToAdd > 0 {
		b.tokens += tokensToAdd
		if b.tokens > b.maxTokens {
			b.tokens = b.maxTokens
		}
		b.lastRefill = now
	}
}

// Router routes messages to appropriate handlers.
type Router struct {
	log *slog.Logger
}

// NewRouter creates a new message router.
func NewRouter(log *slog.Logger) *Router {
	return &Router{log: log}
}

// RouteResult describes how a message should be handled.
type RouteResult struct {
	IsCommand   bool
	CommandName string
	CommandArgs string
	IsChat      bool
}

// Route determines how to handle an incoming message.
func (r *Router) Route(message string) RouteResult {
	message = strings.TrimSpace(message)

	// Check for slash commands
	if strings.HasPrefix(message, "/") {
		parts := strings.SplitN(message, " ", 2)
		cmd := strings.ToLower(parts[0])
		var args string
		if len(parts) > 1 {
			args = parts[1]
		}

		switch cmd {
		case "/help", "/status", "/reset", "/logout", "/history":
			return RouteResult{IsCommand: true, CommandName: cmd, CommandArgs: args}
		}
	}

	// Check for confirmation responses
	upper := strings.ToUpper(strings.TrimSpace(message))
	if upper == "YES" || upper == "NO" || upper == "Y" || upper == "N" {
		return RouteResult{IsCommand: true, CommandName: "/confirm", CommandArgs: upper}
	}

	return RouteResult{IsChat: true}
}
