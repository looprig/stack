// Package devauth is DEVELOPMENT-ONLY identity for a stack running on a
// developer's own machine. Never expose a process using it to anyone else.
//
// Anyone who knows the dev token can sign in as ANY username of the one
// tenant. The credential is "<username>:<token>", presented as a bearer token
// (Authorization: Bearer alice:<token>, for curl and tests) or as the
// CookieName cookie the /dev/login form sets. There is no per-user password,
// no server-side logout and no rate limiting.
//
// The Identity it returns is marked DevelopmentOnly: stack refuses to start
// it without a Logger, and logs a WARN banner on every start.
//
// In production, implement identity.Verifier over your identity provider
// (a signed session cookie, an OIDC access token) and factory.Authorizer
// with your ownership rules; nothing else in a stack composition changes.
package devauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/stack"
)

// CookieName is the browser cookie the /dev/login form sets.
const CookieName = "looprig_dev"

// MinTokenBytes is the shortest dev token New accepts.
const MinTokenBytes = 16

// credentialTTL is how long a verified dev credential is good for.
const credentialTTL = 12 * time.Hour

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ErrShortToken is New's refusal of a token under MinTokenBytes.
var ErrShortToken = errors.New("devauth: the dev token is shorter than MinTokenBytes")

// Dev is one development identity: one tenant, one shared token.
type Dev struct {
	tenant  sessionwire.TenantID
	token   string
	csrfKey []byte
}

// New returns a development identity for tenant. An empty token mints a
// random one (read it back with Token).
func New(tenant sessionwire.TenantID, token string) (*Dev, error) {
	if err := tenant.Validate(); err != nil {
		return nil, fmt.Errorf("devauth: tenant: %w", err)
	}
	if token == "" {
		token = random(24)
	}
	if len(token) < MinTokenBytes {
		return nil, ErrShortToken
	}
	return &Dev{tenant: tenant, token: token, csrfKey: []byte(random(identity.MinCSRFSharedKeyBytes))}, nil
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// Token is the shared dev token.
func (d *Dev) Token() string { return d.token }

// Credential is the bearer credential for user: "<user>:<token>".
func (d *Dev) Credential(user string) string { return user + ":" + d.token }

// Identity is the stack.Identity: the dev verifier, factory.TenantAuthorizer,
// a per-process CSRF key and the dev cookie. It is marked DevelopmentOnly.
func (d *Dev) Identity() stack.Identity {
	return stack.Identity{
		Verifier:        verifier{d},
		Authorizer:      factory.TenantAuthorizer{},
		CSRF:            identity.CSRFConfig{SharedKey: append([]byte(nil), d.csrfKey...)},
		CookieName:      CookieName,
		DevelopmentOnly: true,
	}
}

type verifier struct{ d *Dev }

func (v verifier) VerifyCredential(_ context.Context, credential identity.Credential) (identity.Claims, error) {
	user, token, ok := strings.Cut(credential.Value(), ":")
	if !ok || !usernamePattern.MatchString(user) || !v.d.tokenMatches(token) {
		// Wrapping ErrUnauthenticated makes Factory answer 401; any other
		// error would read as the verifier being unavailable.
		return identity.Claims{}, fmt.Errorf("devauth: credential refused: %w", identity.ErrUnauthenticated)
	}
	return identity.Claims{
		Tenant:    v.d.tenant,
		Subject:   user,
		Kind:      identity.KindActor,
		ExpiresAt: time.Now().Add(credentialTTL),
	}, nil
}

func (d *Dev) tokenMatches(presented string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(d.token)) == 1
}

// Routes serves GET/POST /dev/login (a sign-in form that sets the dev
// cookie) and POST /dev/logout, and hands every other request to next. Pass
// it as stack.Options.UI, wrapping your own UI handler (nil answers 404).
func (d *Dev) Routes(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dev/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		message := ""
		if r.URL.Query().Has("failed") {
			message = `<p style="color:#b00">Wrong username or token.</p>`
		}
		_, _ = fmt.Fprintf(w, `<!doctype html><title>Sign in (dev)</title>
<body style="font-family:system-ui;max-width:28rem;margin:4rem auto">
<h1>Sign in</h1><p><strong>Development only.</strong> Tenant %s. Any username, and the dev token printed at startup.</p>%s
<form method="post" action="/dev/login">
<p><label>Username <input name="user" pattern="[a-z0-9][a-z0-9_\-]{0,31}" required></label></p>
<p><label>Dev token <input name="token" type="password" required></label></p>
<button>Sign in</button></form></body>`, html.EscapeString(string(d.tenant)), message)
	})
	mux.HandleFunc("POST /dev/login", func(w http.ResponseWriter, r *http.Request) {
		user := r.PostFormValue("user")
		if !usernamePattern.MatchString(user) || !d.tokenMatches(r.PostFormValue("token")) {
			http.Redirect(w, r, "/dev/login?failed", http.StatusSeeOther)
			return
		}
		// Secure only over TLS: a development stack is usually plain HTTP on
		// localhost, where a Secure cookie would not be sent back.
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure follows the transport; see above
			Name: CookieName, Value: d.Credential(user), Path: "/", Secure: r.TLS != nil,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(credentialTTL.Seconds()),
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /dev/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure follows the transport, as at login
			Name: CookieName, Value: "", Path: "/", MaxAge: -1, Secure: r.TLS != nil,
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/dev/login", http.StatusSeeOther)
	})
	mux.Handle("/", next)
	return mux
}
