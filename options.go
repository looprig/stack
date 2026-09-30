package stack

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/harness/pkg/rig"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/host/harnessruntime"
	"github.com/looprig/storage"
)

// Options is one single-process composition, described. Validate reports
// every refusal it can make without I/O; Start runs Validate first.
type Options struct {
	// Storage is where every durable record lives. REQUIRED. Use
	// stack/localdisk for one machine, stack/memory for tests, or assemble one
	// over a shared backend (natsstore, pgstore+s3store) for RemoteHosts.
	Storage Storage

	// Tenants are the tenants this process serves, at least one, distinct.
	// Each gets its own harness journal backend (Storage.Journal). With
	// exactly one tenant it is also Factory's default tenant, so a credential
	// that names no tenant is scoped to it.
	Tenants []sessionwire.TenantID

	// Identity authenticates and authorizes every public request. REQUIRED.
	Identity Identity

	// Agents are the agents a create may name, at least one, distinct IDs.
	Agents []Agent

	// Hosts selects where agents run. Nil means InProcess{}.
	Hosts HostMode

	// Live enables live previews (text, and optionally reasoning and tool
	// steps) for every viewer. Nil keeps committed-only publication. Every
	// runtime the stack launches implements the live-options seam, so
	// IncludeReasoning and IncludeToolSteps need nothing else.
	Live *host.LiveTextOptions

	// UI is optional; Factory mounts it as the fallback after /v1, so an
	// unknown API path is still a JSON 404.
	UI http.Handler

	// Origins are the browser origins trusted by CSRF and ClientLink. They
	// are the ONLY source of trusted origins: Identity.CSRF.TrustedOrigins
	// must be empty. At least one is REQUIRED (Factory's CSRF refuses none).
	Origins []string

	// Logger receives operator diagnostics. Optional, except that a
	// development identity (Identity.DevelopmentOnly) refuses to start
	// without one, so its banner cannot be discarded.
	Logger *slog.Logger

	// Limits tunes timings and bounds. Its zero value is the single-process
	// default.
	Limits Limits
}

// Storage is one storage profile: the control backend Factory and Host share,
// one harness journal backend per tenant, and the local directory session
// workspaces are materialized under.
type Storage struct {
	// Control holds the SessionStore both halves use: catalog, command inbox,
	// host registry, gates. REQUIRED. Its Blobs must implement
	// storage.BlobReaderLifecycle (SessionStore refuses one that does not);
	// stack/localdisk adapts fsstore with storage.WithBoundedBlobReaders.
	Control *storage.Composite

	// Journal opens (or returns) the harness journal backend of one tenant.
	// REQUIRED. The stack calls it once per tenant at Start. Harness's
	// journal layout holds one tenant per backend, so it must never return
	// one backend for two tenants.
	Journal func(sessionwire.TenantID) (*storage.Composite, error)

	// Workspaces is the local directory a session's workspace is created
	// under, as <Workspaces>/<tenant>/<session>. REQUIRED and absolute.
	Workspaces string

	// SingleProcess marks a backend only one OS process may open
	// (stack/localdisk). Such storage is refused with RemoteHosts, because a
	// Host in another process could not read the journals it writes.
	SingleProcess bool

	// Close releases the profile. Optional. The stack calls it LAST, after
	// Factory and Host have stopped, and only once.
	Close func() error
}

// Identity is how Factory authenticates and authorizes a request.
type Identity struct {
	// Verifier verifies a presented credential. REQUIRED.
	Verifier identity.Verifier

	// Authorizer decides every public operation. REQUIRED.
	// factory.TenantAuthorizer{} is the stock tenant-boundary choice.
	Authorizer factory.Authorizer

	// CSRF signs browser CSRF tokens. SharedKey is REQUIRED, at least
	// identity.MinCSRFSharedKeyBytes, and must be the same bytes in every
	// replica. TokenTTL zero means 12 hours. TrustedOrigins must be empty:
	// use Options.Origins.
	CSRF identity.CSRFConfig

	// CookieName replaces the browser session cookie name. Optional.
	CookieName string

	// DevelopmentOnly marks a development identity (stack/devauth sets it).
	// Validate then requires Options.Logger, and Start logs a WARN banner on
	// every start saying this process accepts development credentials.
	DevelopmentOnly bool
}

// Agent is one agent a create may name, and how to build its rig.
type Agent struct {
	// ID is what a create request names. REQUIRED, distinct.
	ID sessionwire.AgentID

	// Compatibility names this agent BUILD. A session is restored only onto
	// the same id, so change it when a new build cannot safely replay an old
	// journal. REQUIRED.
	Compatibility department.CompatibilityID

	// Capabilities is what the agent declares about placement. The stack
	// places every session pooled, so SupportsPooled is set for you;
	// AdmissionWeight zero means 1; Recovery is forced on (the harness
	// runtime supplies both capabilities by construction). CaptureSafety is
	// REQUIRED and must permit pooling: department.CaptureSafetyStreaming
	// when every tool's output is small or streamed, or
	// department.CaptureSafetyBoundedMaterialized under a finite limit.
	// RequiresCheckpoint is refused: stack v0.1.0 has no checkpointer.
	Capabilities department.Capabilities

	// Define builds the rig one launch runs on. REQUIRED. It is called for
	// every create and every restore (rig.Define fixes the journal store and
	// workspace, so a rig is per launch); memoise per tenant if your rig does
	// not depend on the workspace. Pass Binding.Journal to
	// rig.WithSessionStore, or the session journals nowhere Host can read.
	Define func(context.Context, Binding) (*rig.Rig, error)

	// Decode reads a create/input body. Optional; nil means Core's
	// InputRequest (harnessruntime.DecodeInputBlocks), which is what Factory
	// admits.
	Decode harnessruntime.BlockDecoder
}

// Binding is what a rig needs that only the stack knows.
type Binding struct {
	Tenant  sessionwire.TenantID
	Session sessionwire.SessionID
	// Journal is the tenant's harness session store; pass it to
	// rig.WithSessionStore. Its ToolResultObjects() feeds
	// rig.WithToolResultObjects.
	Journal *harnessstore.Store
	// WorkspaceRoot is this session's materialized workspace directory,
	// <Storage.Workspaces>/<tenant>/<session>. It is "" unless the agent
	// declares Capabilities.RequiresWorkspace: Host materializes a workspace
	// only for a target that asks for one.
	WorkspaceRoot string
	// Restore is true when the launch restores an existing conversation.
	Restore bool
}

// HostMode selects where agents run: InProcess or RemoteHosts.
type HostMode interface{ hostMode() }

// InProcess runs one pooled Host inside this process. Factory still reaches
// it over a loopback HostLink WebSocket, exactly as it would across
// machines, so moving to RemoteHosts changes nothing else.
type InProcess struct {
	// Listen is the HostLink listen address. It must be loopback; "" means
	// "127.0.0.1:0".
	Listen string
	// HostID names the Host. "" means "local".
	HostID sessionwire.HostID
	// Capacity bounds resident sessions. Zero means 16.
	Capacity uint64
}

// RemoteHosts runs Factory only; Hosts run in other processes via ServeHost
// over the same (shared) Storage, Tenants and Agents.
// Options.Limits.HostLinkCredential is the credential Factory presents to
// them.
type RemoteHosts struct{}

func (InProcess) hostMode()   {}
func (RemoteHosts) hostMode() {}

// Limits tunes the composition. Every zero field takes its default.
type Limits struct {
	// Host tunes the in-process Host (and ServeHost's).
	Host HostLimits

	// Reconcile, ClientLink, HostLink, HTTP and Routes replace Factory's
	// defaults when non-nil. MaxCommandBodyBytes is Routes'.
	Reconcile  *factory.ReconcileLimits
	ClientLink *factory.ClientLinkLimits
	HostLink   *factory.HostLinkLimits
	HTTP       *factory.HTTPLimits
	Routes     *factory.RouteLimits

	// HostLinkCredential is the service token Factory presents to remote
	// Hosts. REQUIRED under RemoteHosts (at least MinHostLinkCredentialBytes)
	// and refused under InProcess, where the stack mints a random one per
	// process.
	HostLinkCredential string
}

// MinHostLinkCredentialBytes is the shortest HostLinkCredential accepted.
const MinHostLinkCredentialBytes = 32

// HostLimits tunes one Host. Zero fields take the single-process defaults
// named on each.
type HostLimits struct {
	WarmTTL           time.Duration // 10m: idle resident sessions are released after it
	RegistryHeartbeat time.Duration // 2s
	RegistryExpiry    time.Duration // 10s; at least host.MinHeartbeatsBeforeExpiry heartbeats
	ClaimTTL          time.Duration // 5s
	ApplyDeadline     time.Duration // 60s; at least host.MinClaimAttemptsBeforeDeadline claims
	CommandQueueSize  int           // 16; at most host.MaxCommandQueueSize
	ReconcileInterval time.Duration // 1s
	ReconcileBatch    int           // 32; at most host.MaxReconcileBatch

	Link  host.LinkOptions  // zero fields: 64 bindings per link, 256 bindings, 4 links per tenant
	Drain host.DrainOptions // zero fields: grace 10s, idle boundary 5s, publish bound 2s

	CompatibilityTimeout time.Duration // 20s
	WorkPoll             time.Duration // 1s

	// MaxCommandBodyBytes bounds a referenced command body. Zero is Host's
	// 8 MiB default.
	MaxCommandBodyBytes int64
}

// HostOptions runs one Host process for a RemoteHosts deployment. Storage,
// Tenants and Agents are the same values the Factory process uses.
type HostOptions struct {
	Storage Storage
	Tenants []sessionwire.TenantID
	Agents  []Agent

	// HostID names this Host. REQUIRED.
	HostID sessionwire.HostID
	// Generation is this run's incarnation. Zero means the next value of the
	// counter the stack keeps in Storage.Control for HostID.
	Generation uint64
	// Base is the BARE HostLink base Factory dials (scheme and authority,
	// no path); Factory derives each tenant's address from it. REQUIRED.
	Base sessionwire.InternalEndpoint
	// Credential verifies the token Factory presents. REQUIRED;
	// TokenVerifier(Limits.HostLinkCredential) is the matching choice.
	Credential host.AuthVerifier
	// Capacity bounds resident sessions. Zero means 16.
	Capacity uint64
	Live     *host.LiveTextOptions
	Limits   HostLimits
	Logger   *slog.Logger
}
