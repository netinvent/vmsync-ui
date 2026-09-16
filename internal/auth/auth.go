/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// Package auth handles who is using the UI and what they may do.
//
// Accounts live in the UI's config file rather than a database: this is an
// internal tool for a small team, and a file is one less moving part to back
// up and one more thing an operator can fix from a shell during an incident.
//
// Attribution only means anything if accounts are not shared. "Who failed
// over production at 3am" is a question that gets asked eventually, and a
// single shared admin login makes it unanswerable.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Role is what an account may do.
type Role string

const (
	// RoleReadOnly may look at everything and run the verification modes
	// that do not suspend the source domain.
	RoleReadOnly Role = "readonly"
	// RoleAdmin may do everything, including the verify modes that suspend
	// the source, schedule changes, promotions and inversions.
	RoleAdmin Role = "admin"
)

func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleReadOnly:
		return RoleReadOnly, nil
	case RoleAdmin:
		return RoleAdmin, nil
	default:
		return "", fmt.Errorf("unknown role %q: must be %q or %q", s, RoleReadOnly, RoleAdmin)
	}
}

// Account is one person who can sign in.
type Account struct {
	Username string `json:"username"`
	// PasswordHash is the encoded PBKDF2 digest produced by HashPassword.
	// A plaintext password is never stored or accepted.
	PasswordHash string `json:"password_hash"`
	Role         Role   `json:"role"`
}

// User is an authenticated session's identity.
type User struct {
	Username string
	Role     Role
	// CSRF is a per-session token embedded in every form that changes
	// something and checked on submission. Without it, a page on another
	// site could make an authenticated operator's browser revoke an agent,
	// since the session cookie would be sent automatically.
	CSRF string
}

// IsAdmin reports whether this user may take actions that change something.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// MayRunSuspendingVerify reports whether this user may start a verification
// mode that suspends the source VM.
//
// This is the one capability split that is easy to get wrong by accident.
// Which modes suspend is NOT decided here -- see store.SuspendsSource, which
// is written as an allowlist of the non-suspending ones precisely so that a
// mode this build has never heard of is treated as suspending. Offering
// "just a verification" to a read-only user would hand them a
// production-impacting action under a harmless-sounding name.
func (u User) MayRunSuspendingVerify() bool { return u.IsAdmin() }

// --- password hashing -----------------------------------------------------

// pbkdf2Iterations is deliberately high: these hashes sit in a config file
// on an operations host, and the cost is paid once per sign-in by a human.
const pbkdf2Iterations = 600_000

// HashPassword returns an encoded PBKDF2-HMAC-SHA256 digest, in the form
// pbkdf2_sha256$<iterations>$<salt-b64>$<hash-b64>.
//
// PBKDF2 from the standard library rather than bcrypt or argon2 from
// x/crypto: this program deliberately has no dependencies outside stdlib,
// and a well-iterated PBKDF2 is a sound choice for a handful of operator
// accounts.
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", fmt.Errorf("derive key: %w", err)
	}
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches an encoded digest.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- sessions -------------------------------------------------------------

const SessionCookie = "vmsync_ui_session"

type session struct {
	user      User
	expiresAt time.Time
}

// Manager authenticates sign-ins and tracks sessions.
//
// Sessions are in memory only. A UI restart signs everyone out, which is
// acceptable for an operations console and avoids persisting anything that
// grants access -- there is no session file to leak or to have to expire.
type Manager struct {
	accounts map[string]Account
	ttl      time.Duration

	mu       sync.Mutex
	sessions map[string]session
}

func NewManager(accounts []Account, ttl time.Duration) (*Manager, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no accounts configured: the UI would be unusable")
	}
	byName := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		if a.Username == "" || a.PasswordHash == "" {
			return nil, fmt.Errorf("account %q is missing a username or password hash", a.Username)
		}
		if _, err := ParseRole(string(a.Role)); err != nil {
			return nil, fmt.Errorf("account %q: %w", a.Username, err)
		}
		if _, dup := byName[a.Username]; dup {
			return nil, fmt.Errorf("duplicate account %q", a.Username)
		}
		byName[a.Username] = a
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Manager{accounts: byName, ttl: ttl, sessions: map[string]session{}}, nil
}

// SignIn validates credentials and returns a new session token.
//
// An unknown username still runs a hash comparison against a dummy digest,
// so the response time does not reveal which usernames exist.
func (m *Manager) SignIn(username, password string) (string, User, bool) {
	acct, known := m.accounts[username]
	if !known {
		acct.PasswordHash = dummyHash
	}
	if !VerifyPassword(acct.PasswordHash, password) || !known {
		return "", User{}, false
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", User{}, false
	}
	csrfRaw := make([]byte, 32)
	if _, err := rand.Read(csrfRaw); err != nil {
		return "", User{}, false
	}
	token := hex.EncodeToString(raw)
	user := User{Username: acct.Username, Role: acct.Role, CSRF: hex.EncodeToString(csrfRaw)}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = session{user: user, expiresAt: time.Now().Add(m.ttl)}
	return token, user, true
}

func (m *Manager) SignOut(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

// User resolves a session token, dropping it if it has expired.
func (m *Manager) User(token string) (User, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok {
		return User{}, false
	}
	if time.Now().After(s.expiresAt) {
		delete(m.sessions, token)
		return User{}, false
	}
	return s.user, true
}

// FromRequest resolves the signed-in user from a request's session cookie.
func (m *Manager) FromRequest(r *http.Request) (User, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	return m.User(c.Value)
}

// CheckCSRF reports whether a form submission carries this user's own CSRF
// token. Compared in constant time out of habit rather than necessity.
func (u User) CheckCSRF(submitted string) bool {
	if u.CSRF == "" || submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(u.CSRF), []byte(submitted)) == 1
}

// dummyHash is a real, valid digest of an unguessable value, used to keep
// the sign-in path's timing the same for unknown usernames as for known
// ones. Generated once at init rather than hardcoded so it cannot become a
// known-plaintext.
var dummyHash = func() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	h, err := HashPassword(hex.EncodeToString(raw))
	if err != nil {
		return ""
	}
	return h
}()
