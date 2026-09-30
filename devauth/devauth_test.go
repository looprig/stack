package devauth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/looprig/factory/identity"
	"github.com/looprig/stack"
	"github.com/looprig/stack/devauth"
)

const token = "devauth-test-token-0123"

func credential(t *testing.T, value string) identity.Credential {
	t.Helper()
	return identity.NewCredential(identity.SourceBearer, value)
}

func TestNew(t *testing.T) {
	if _, err := devauth.New("dev", "short"); !errors.Is(err, devauth.ErrShortToken) {
		t.Fatalf("New(short) = %v, want ErrShortToken", err)
	}
	if _, err := devauth.New("", token); err == nil {
		t.Fatal("New accepted an empty tenant")
	}
	minted, err := devauth.New("dev", "")
	if err != nil || len(minted.Token()) < devauth.MinTokenBytes {
		t.Fatalf("New minted %q, %v", minted.Token(), err)
	}
	other, _ := devauth.New("dev", "")
	if other.Token() == minted.Token() {
		t.Fatal("two minted tokens are equal")
	}
}

func TestVerifier(t *testing.T) {
	dev, err := devauth.New("dev", token)
	if err != nil {
		t.Fatal(err)
	}
	verifier := dev.Identity().Verifier
	claims, err := verifier.VerifyCredential(context.Background(), credential(t, dev.Credential("alice")))
	if err != nil {
		t.Fatal(err)
	}
	if claims.Tenant != "dev" || claims.Subject != "alice" || claims.Kind != identity.KindActor || claims.ExpiresAt.IsZero() {
		t.Fatalf("claims = %+v", claims)
	}
	for _, bad := range []string{"alice", "alice:" + token + "x", "Alice:" + token, ":" + token, "alice:"} {
		if _, err := verifier.VerifyCredential(context.Background(), credential(t, bad)); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("credential %q: err = %v, want ErrUnauthenticated", bad, err)
		}
	}
}

func TestIdentityIsMarkedAndStartsOnlyWithALogger(t *testing.T) {
	dev, err := devauth.New("dev", token)
	if err != nil {
		t.Fatal(err)
	}
	id := dev.Identity()
	if !id.DevelopmentOnly || id.CookieName != devauth.CookieName || len(id.CSRF.SharedKey) < identity.MinCSRFSharedKeyBytes {
		t.Fatalf("Identity = %+v", id)
	}
	// The Logger refusal is exercised in stack's own table; here, prove the
	// mark reaches it.
	var refusal *stack.OptionError
	full := validWith(t, id, nil)
	if err := stack.Validate(full); !errors.As(err, &refusal) || refusal.Field != "Logger" {
		t.Fatalf("Validate(dev identity, no logger) = %v, want a Logger refusal", err)
	}
	if err := stack.Validate(validWith(t, id, slog.New(slog.NewTextHandler(io.Discard, nil)))); err != nil {
		t.Fatalf("Validate(dev identity, logger) = %v", err)
	}
}

func TestRoutes(t *testing.T) {
	dev, err := devauth.New("dev", token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(dev.Routes(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "next")
	})))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(server.URL + "/dev/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Development only") {
		t.Fatalf("login page = %s", body)
	}

	resp, err = client.PostForm(server.URL+"/dev/login", url.Values{"user": {"alice"}, "token": {"wrong-token-wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/dev/login?failed" || len(resp.Cookies()) != 0 {
		t.Fatalf("wrong token: Location %q, cookies %v", loc, resp.Cookies())
	}

	resp, err = client.PostForm(server.URL+"/dev/login", url.Values{"user": {"alice"}, "token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != devauth.CookieName || cookies[0].Value != dev.Credential("alice") || !cookies[0].HttpOnly {
		t.Fatalf("login cookies = %v", cookies)
	}

	resp, err = client.Get(server.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "next" {
		t.Fatalf("other paths = %q, want the next handler", body)
	}
}
