package stack

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type stubVerifier struct{}

func (stubVerifier) VerifyCredential(context.Context, identity.Credential) (identity.Claims, error) {
	return identity.Claims{}, identity.ErrUnauthenticated
}

func stubDefine(context.Context, Binding) (*rig.Rig, error) { return nil, errors.New("unused") }

// validOptions is a composition Validate accepts; every case spoils one field.
func validOptions() Options {
	return Options{
		Storage: Storage{
			Control:    memstore.New(),
			Journal:    func(sessionwire.TenantID) (*storage.Composite, error) { return memstore.New(), nil },
			Workspaces: "/tmp/stack-validate",
		},
		Tenants: []sessionwire.TenantID{"acme", "globex"},
		Identity: Identity{
			Verifier:   stubVerifier{},
			Authorizer: factory.TenantAuthorizer{},
			CSRF:       identity.CSRFConfig{SharedKey: []byte(strings.Repeat("k", identity.MinCSRFSharedKeyBytes))},
		},
		Agents: []Agent{{
			ID:            "assistant",
			Compatibility: "stack-test/v1",
			Capabilities:  department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
			Define:        stubDefine,
		}},
		Origins: []string{"http://127.0.0.1:8080"},
	}
}

// blobsOnly hides every optional capability of a Blobs, as fsstore's does.
type blobsOnly struct{ storage.Blobs }

type otherMode struct{ InProcess }

// zeroBound is a lifecycle provider declaring a zero close bound.
type zeroBound struct{ storage.Blobs }

func (zeroBound) BlobReaderCloseBound() time.Duration { return 0 }

func TestValidateAcceptsTheBaselineAndBoundaries(t *testing.T) {
	for name, edit := range map[string]func(*Options){
		"baseline":         func(*Options) {},
		"one tenant":       func(o *Options) { o.Tenants = o.Tenants[:1] },
		"explicit process": func(o *Options) { o.Hosts = InProcess{Listen: "127.0.0.1:0", HostID: "h1", Capacity: 4} },
		"localhost":        func(o *Options) { o.Hosts = InProcess{Listen: "localhost:0"} },
		"ipv6 loopback":    func(o *Options) { o.Hosts = InProcess{Listen: "[::1]:0"} },
		"remote hosts": func(o *Options) {
			o.Hosts = RemoteHosts{}
			o.Limits.HostLinkCredential = strings.Repeat("c", MinHostLinkCredentialBytes)
		},
		"bounded capture": func(o *Options) {
			o.Agents[0].Capabilities.CaptureSafety = department.CaptureSafetyBoundedMaterialized
		},
		"dev identity with logger": func(o *Options) {
			o.Identity.DevelopmentOnly = true
			o.AllowDevelopmentIdentity = true
			o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		},
		"live burst of one frame": func(o *Options) { o.Live = &host.LiveTextOptions{BurstBytes: 4 << 10} },
		"link heartbeat": func(o *Options) {
			o.Limits.Host.Link.PingInterval, o.Limits.Host.Link.PongTimeout = 2*time.Second, time.Second
		},
		"expiry exactly the heartbeat margin": func(o *Options) {
			o.Limits.Host.RegistryHeartbeat = time.Second
			o.Limits.Host.RegistryExpiry = time.Second * host.MinHeartbeatsBeforeExpiry
		},
		"deadline exactly the claim margin": func(o *Options) {
			o.Limits.Host.ClaimTTL = time.Second
			o.Limits.Host.ApplyDeadline = time.Second * host.MinClaimAttemptsBeforeDeadline
		},
		"queue at bound":        func(o *Options) { o.Limits.Host.CommandQueueSize = host.MaxCommandQueueSize },
		"batch at bound":        func(o *Options) { o.Limits.Host.ReconcileBatch = host.MaxReconcileBatch },
		"factory limits":        func(o *Options) { l := factory.DefaultReconcileLimits(); o.Limits.Reconcile = &l },
		"live previews":         func(o *Options) { o.Live = &host.LiveTextOptions{IncludeReasoning: true, IncludeToolSteps: true} },
		"body limit in range":   func(o *Options) { o.Limits.Host.MaxCommandBodyBytes = 1 << 20 },
		"agent weight declared": func(o *Options) { o.Agents[0].Capabilities.AdmissionWeight = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			o := validOptions()
			edit(&o)
			if err := Validate(o); err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
		})
	}
}

func TestValidateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		edit  func(*Options)
	}{
		{"no control", "Storage.Control", func(o *Options) { o.Storage.Control = nil }},
		{"no control KV", "Storage.Control.KV", func(o *Options) { o.Storage.Control.KV = nil }},
		{"no control ledger", "Storage.Control.Ledger", func(o *Options) { o.Storage.Control.Ledger = nil }},
		{"no control leaser", "Storage.Control.Leaser", func(o *Options) { o.Storage.Control.Leaser = nil }},
		{"no control blobs", "Storage.Control.Blobs", func(o *Options) { o.Storage.Control.Blobs = nil }},
		{"no control ordered index", "Storage.Control.OrderedIndex", func(o *Options) { o.Storage.Control.OrderedIndex = nil }},
		{"non-positive close bound", "Storage.Control.Blobs", func(o *Options) {
			o.Storage.Control.Blobs = zeroBound{o.Storage.Control.Blobs}
		}},
		{"unbounded blobs", "Storage.Control.Blobs", func(o *Options) {
			o.Storage.Control.Blobs = blobsOnly{o.Storage.Control.Blobs}
		}},
		{"no journal", "Storage.Journal", func(o *Options) { o.Storage.Journal = nil }},
		{"no workspaces", "Storage.Workspaces", func(o *Options) { o.Storage.Workspaces = "" }},
		{"relative workspaces", "Storage.Workspaces", func(o *Options) { o.Storage.Workspaces = "work" }},
		{"no tenants", "Tenants", func(o *Options) { o.Tenants = nil }},
		{"empty tenant", "Tenants[1]", func(o *Options) { o.Tenants[1] = "" }},
		{"unroutable tenant", "Tenants[0]", func(o *Options) { o.Tenants[0] = "a/b" }},
		{"duplicate tenant", "Tenants[1]", func(o *Options) { o.Tenants[1] = o.Tenants[0] }},
		{"no verifier", "Identity.Verifier", func(o *Options) { o.Identity.Verifier = nil }},
		{"no authorizer", "Identity.Authorizer", func(o *Options) { o.Identity.Authorizer = nil }},
		{"short csrf key", "Identity.CSRF.SharedKey", func(o *Options) {
			o.Identity.CSRF.SharedKey = o.Identity.CSRF.SharedKey[:identity.MinCSRFSharedKeyBytes-1]
		}},
		{"negative csrf ttl", "Identity.CSRF.TokenTTL", func(o *Options) { o.Identity.CSRF.TokenTTL = -time.Second }},
		{"two origin lists", "Identity.CSRF.TrustedOrigins", func(o *Options) {
			o.Identity.CSRF.TrustedOrigins = []string{"http://127.0.0.1:8080"}
		}},
		{"no origins", "Origins", func(o *Options) { o.Origins = nil }},
		{"bad origin", "Origins", func(o *Options) { o.Origins = []string{"http://example.com/path"} }},
		{"duplicate origin", "Origins", func(o *Options) {
			o.Origins = []string{"http://127.0.0.1:8080", "http://127.0.0.1:8080"}
		}},
		{"dev identity without opt-in", "AllowDevelopmentIdentity", func(o *Options) {
			o.Identity.DevelopmentOnly = true
			o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}},
		{"dev identity without logger", "Logger", func(o *Options) {
			o.Identity.DevelopmentOnly, o.AllowDevelopmentIdentity = true, true
		}},
		{"dev identity with an error-level logger", "Logger", func(o *Options) {
			o.Identity.DevelopmentOnly, o.AllowDevelopmentIdentity = true, true
			o.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		}},
		{"dev identity with a discarding logger", "Logger", func(o *Options) {
			o.Identity.DevelopmentOnly, o.AllowDevelopmentIdentity = true, true
			o.Logger = slog.New(slog.DiscardHandler)
		}},
		{"no agents", "Agents", func(o *Options) { o.Agents = nil }},
		{"empty agent id", "Agents[0].ID", func(o *Options) { o.Agents[0].ID = "" }},
		{"duplicate agent", "Agents[1].ID", func(o *Options) {
			second := o.Agents[0]
			second.Compatibility = "stack-test/v2"
			o.Agents = append(o.Agents, second)
		}},
		{"no compatibility", "Agents[0].Compatibility", func(o *Options) { o.Agents[0].Compatibility = "" }},
		{"no define", "Agents[0].Define", func(o *Options) { o.Agents[0].Define = nil }},
		{"checkpoint", "Agents[0].Capabilities.RequiresCheckpoint", func(o *Options) {
			o.Agents[0].Capabilities.RequiresCheckpoint = true
		}},
		{"undeclared capture", "Agents[0].Capabilities.CaptureSafety", func(o *Options) {
			o.Agents[0].Capabilities.CaptureSafety = ""
		}},
		{"unbounded capture", "Agents[0].Capabilities.CaptureSafety", func(o *Options) {
			o.Agents[0].Capabilities.CaptureSafety = department.CaptureSafetyUnboundedMaterialized
		}},
		{"unknown host mode", "Hosts", func(o *Options) { o.Hosts = otherMode{} }},
		{"public listener", "Hosts.Listen", func(o *Options) { o.Hosts = InProcess{Listen: "0.0.0.0:0"} }},
		{"lan listener", "Hosts.Listen", func(o *Options) { o.Hosts = InProcess{Listen: "192.168.1.2:0"} }},
		{"bad listener", "Hosts.Listen", func(o *Options) { o.Hosts = InProcess{Listen: "127.0.0.1"} }},
		{"bad host id", "Hosts.HostID", func(o *Options) { o.Hosts = InProcess{HostID: "\xff"} }},
		{"in-process credential", "Limits.HostLinkCredential", func(o *Options) {
			o.Limits.HostLinkCredential = strings.Repeat("c", MinHostLinkCredentialBytes)
		}},
		{"remote hosts on local disk", "Hosts", func(o *Options) {
			o.Hosts = RemoteHosts{}
			o.Storage.SingleProcess = true
			o.Limits.HostLinkCredential = strings.Repeat("c", MinHostLinkCredentialBytes)
		}},
		{"remote hosts short credential", "Limits.HostLinkCredential", func(o *Options) {
			o.Hosts = RemoteHosts{}
			o.Limits.HostLinkCredential = strings.Repeat("c", MinHostLinkCredentialBytes-1)
		}},
		{"negative live rate", "Live.RateBytesPerSecond", func(o *Options) {
			o.Live = &host.LiveTextOptions{RateBytesPerSecond: -1}
		}},
		{"negative live flush", "Live.FlushInterval", func(o *Options) { o.Live = &host.LiveTextOptions{FlushInterval: -1} }},
		{"live burst below one frame", "Live.BurstBytes", func(o *Options) { o.Live = &host.LiveTextOptions{BurstBytes: 1} }},
		{"live burst over the queue", "Live.BurstBytes", func(o *Options) { o.Live = &host.LiveTextOptions{BurstBytes: 1 << 20} }},
		{"live rate over the queue", "Live.RateBytesPerSecond", func(o *Options) {
			o.Live = &host.LiveTextOptions{RateBytesPerSecond: 1 << 20}
		}},
		{"negative max bindings", "Limits.Host.Link.MaxBindings", func(o *Options) { o.Limits.Host.Link.MaxBindings = -1 }},
		{"negative bindings per link", "Limits.Host.Link.MaxBindingsPerLink", func(o *Options) {
			o.Limits.Host.Link.MaxBindingsPerLink = -1
		}},
		{"negative tenant links", "Limits.Host.Link.MaxTenantLinks", func(o *Options) { o.Limits.Host.Link.MaxTenantLinks = -1 }},
		{"ping without pong", "Limits.Host.Link.PongTimeout", func(o *Options) { o.Limits.Host.Link.PingInterval = 2 * time.Second }},
		{"sub-second ping", "Limits.Host.Link.PingInterval", func(o *Options) {
			o.Limits.Host.Link.PingInterval, o.Limits.Host.Link.PongTimeout = 500*time.Millisecond, 100*time.Millisecond
		}},
		{"pong not below ping", "Limits.Host.Link.PongTimeout", func(o *Options) {
			o.Limits.Host.Link.PingInterval, o.Limits.Host.Link.PongTimeout = 2*time.Second, 2*time.Second
		}},
		{"negative drain grace", "Limits.Host.Drain.Grace", func(o *Options) { o.Limits.Host.Drain.Grace = -1 }},
		{"negative idle boundary", "Limits.Host.Drain.IdleBoundary", func(o *Options) { o.Limits.Host.Drain.IdleBoundary = -1 }},
		{"negative publish bound", "Limits.Host.Drain.PublishBound", func(o *Options) { o.Limits.Host.Drain.PublishBound = -1 }},
		{"negative warm ttl", "Limits.Host.WarmTTL", func(o *Options) { o.Limits.Host.WarmTTL = -1 }},
		{"expiry below heartbeat margin", "Limits.Host.RegistryExpiry", func(o *Options) {
			o.Limits.Host.RegistryHeartbeat = time.Second
			o.Limits.Host.RegistryExpiry = time.Second*host.MinHeartbeatsBeforeExpiry - 1
		}},
		{"deadline below claim margin", "Limits.Host.ApplyDeadline", func(o *Options) {
			o.Limits.Host.ClaimTTL = time.Second
			o.Limits.Host.ApplyDeadline = time.Second*host.MinClaimAttemptsBeforeDeadline - 1
		}},
		{"default deadline under a long claim", "Limits.Host.ApplyDeadline", func(o *Options) {
			o.Limits.Host.ClaimTTL = time.Minute
		}},
		{"queue over bound", "Limits.Host.CommandQueueSize", func(o *Options) {
			o.Limits.Host.CommandQueueSize = host.MaxCommandQueueSize + 1
		}},
		{"batch over bound", "Limits.Host.ReconcileBatch", func(o *Options) {
			o.Limits.Host.ReconcileBatch = host.MaxReconcileBatch + 1
		}},
		{"body limit inside inbox", "Limits.Host.MaxCommandBodyBytes", func(o *Options) {
			o.Limits.Host.MaxCommandBodyBytes = 64 << 10
		}},
		{"body limit over runtime", "Limits.Host.MaxCommandBodyBytes", func(o *Options) {
			o.Limits.Host.MaxCommandBodyBytes = 16<<20 + 1
		}},
		{"reconcile pass not below claim", "Limits.Reconcile", func(o *Options) {
			l := factory.DefaultReconcileLimits()
			l.PassTimeout = l.ClaimTTL
			o.Limits.Reconcile = &l
		}},
		{"zero http limits", "Limits.HTTP", func(o *Options) { o.Limits.HTTP = &factory.HTTPLimits{} }},
		{"zero client link limits", "Limits.ClientLink", func(o *Options) { o.Limits.ClientLink = &factory.ClientLinkLimits{} }},
		{"zero host link limits", "Limits.HostLink", func(o *Options) { o.Limits.HostLink = &factory.HostLinkLimits{} }},
		{"zero route limits", "Limits.Routes", func(o *Options) { o.Limits.Routes = &factory.RouteLimits{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := validOptions()
			control := *o.Storage.Control // edits must not reach another case's store
			o.Storage.Control = &control
			tc.edit(&o)
			err := Validate(o)
			var refusal *OptionError
			if !errors.As(err, &refusal) {
				t.Fatalf("Validate = %v, want *OptionError", err)
			}
			if refusal.Field != tc.field {
				t.Fatalf("Validate refused %q (%v), want field %q", refusal.Field, err, tc.field)
			}
			if !strings.HasPrefix(err.Error(), "stack: "+tc.field+" ") {
				t.Fatalf("message %q does not lead with the field", err)
			}
		})
	}
}

func TestValidateHostOptionsRefusals(t *testing.T) {
	valid := func() HostOptions {
		o := validOptions()
		return HostOptions{
			Storage: o.Storage, Tenants: o.Tenants, Agents: o.Agents,
			HostID: "remote-1", Base: "ws://10.0.0.7:9000", Credential: TokenVerifier(strings.Repeat("c", 32)),
		}
	}
	if err := validateHostOptions(valid()); err != nil {
		t.Fatalf("validateHostOptions(valid) = %v", err)
	}
	for _, tc := range []struct {
		name  string
		field string
		edit  func(*HostOptions)
	}{
		{"no storage", "Storage.Control", func(o *HostOptions) { o.Storage = Storage{} }},
		{"no tenants", "Tenants", func(o *HostOptions) { o.Tenants = nil }},
		{"no agents", "Agents", func(o *HostOptions) { o.Agents = nil }},
		{"no host id", "HostID", func(o *HostOptions) { o.HostID = "" }},
		{"no base", "Base", func(o *HostOptions) { o.Base = "" }},
		{"base with path", "Base", func(o *HostOptions) { o.Base = "ws://10.0.0.7:9000/pods/7" }},
		{"base naming a tenant", "Base", func(o *HostOptions) { o.Base = "ws://10.0.0.7:9000/hostlink/acme" }},
		{"no credential", "Credential", func(o *HostOptions) { o.Credential = nil }},
		{"negative live burst", "Live.BurstBytes", func(o *HostOptions) { o.Live = &host.LiveTextOptions{BurstBytes: -1} }},
		{"expiry below margin", "Limits.RegistryExpiry", func(o *HostOptions) {
			o.Limits.RegistryHeartbeat = 5 * time.Second
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := valid()
			tc.edit(&o)
			var refusal *OptionError
			if err := validateHostOptions(o); !errors.As(err, &refusal) || refusal.Field != tc.field {
				t.Fatalf("validateHostOptions = %v, want a refusal of %q", err, tc.field)
			}
		})
	}
}

func TestOptionErrorUnwrapsItsCause(t *testing.T) {
	o := validOptions()
	o.Tenants[0] = ""
	err := Validate(o)
	var id *sessionwire.IDValidationError
	if !errors.As(err, &id) {
		t.Fatalf("Validate = %v, want it to wrap Core's *IDValidationError", err)
	}
}
