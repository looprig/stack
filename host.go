package stack

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/host/harnessruntime"
	"github.com/looprig/sessionstore"
	"github.com/looprig/storage"
)

// The journal binding every session the stack creates is pinned to. Factory
// records it in the session's immutable catalog entry; Host's evidence router
// and Factory's journal resolver are keyed on it. It is fixed, not
// configurable: every stack names the same one, so any stack process over the
// same storage can read any stack session.
const (
	JournalBindingID      = "stack-journal"
	JournalBindingVersion = "v1"
)

// namespaceLayout is the durable object prefix a session's runtime writes
// under.
func namespaceLayout(tenant sessionwire.TenantID, session sessionwire.SessionID) string {
	return string(tenant) + "/" + string(session)
}

// hostPlan is everything composeHost needs, resolved.
type hostPlan struct {
	hostID     sessionwire.HostID
	base       sessionwire.InternalEndpoint
	generation uint64
	capacity   uint64
	credential host.AuthVerifier
	live       *host.LiveTextOptions
	limits     HostLimits
	logger     *slog.Logger

	control    *storage.Composite
	workspaces string
	tenants    []sessionwire.TenantID
	journals   map[sessionwire.TenantID]*harnessstore.Store
	agents     []Agent
}

// composeHost builds, but does not start, one pooled Host.
func composeHost(ctx context.Context, plan hostPlan) (*host.Service, error) {
	registrations := make([]department.Registration, 0, len(plan.agents))
	for _, agent := range plan.agents {
		target, err := agentTarget(agent, plan.journals)
		if err != nil {
			return nil, fmt.Errorf("stack: agent %q: %w", agent.ID, err)
		}
		registrations = append(registrations, harnessruntime.Registration(agent.ID, target))
	}
	evidence := make(map[host.EvidenceKey]sessionstore.DispositionEvidenceReader, len(plan.journals))
	for tenant, journal := range plan.journals {
		evidence[host.EvidenceKey{TenantID: tenant, StorageBindingID: JournalBindingID}] = journal
	}
	limits := plan.limits.resolved()
	capacity := plan.capacity
	if capacity == 0 {
		capacity = defaultCapacity
	}
	logger := plan.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return host.Compose(ctx, host.Composition{
		MaxCommandBodyBytes: limits.MaxCommandBodyBytes,
		Options: host.Options{
			HostID:            plan.hostID,
			InternalEndpoint:  plan.base,
			IsolationClass:    sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement:         sessionwire.HostPlacementPooled,
			Capacity:          capacity,
			WarmTTL:           limits.WarmTTL,
			RegistryHeartbeat: limits.RegistryHeartbeat,
			RegistryExpiry:    limits.RegistryExpiry,
			ClaimTTL:          limits.ClaimTTL,
			ApplyDeadline:     limits.ApplyDeadline,
			CommandQueueSize:  limits.CommandQueueSize,
			ReconcileInterval: limits.ReconcileInterval,
			ReconcileBatch:    limits.ReconcileBatch,
		},
		Generation:           plan.generation,
		Link:                 limits.Link,
		Drain:                limits.Drain,
		LiveText:             plan.live,
		CompatibilityTimeout: limits.CompatibilityTimeout,
		WorkPoll:             limits.WorkPoll,
		RuntimeProfile:       host.RuntimeProfileDurable,
		Collaborators: host.Collaborators{
			Backend:       plan.control,
			JournalStores: evidence,
			Registrar: host.RegistrarFunc(func(context.Context) ([]department.Registration, error) {
				return registrations, nil
			}),
			Checkpointer:    noCheckpoint{},
			Auth:            tenantScopedVerifier{tenants: plan.tenants, inner: plan.credential},
			Workspaces:      localWorkspaces{root: plan.workspaces},
			NamespaceLayout: namespaceLayout,
			Logger:          logger,
		},
	})
}

// agentTarget is harnessruntime.Target over the agent's Define, resolving
// the tenant's journal store for every launch.
func agentTarget(agent Agent, journals map[sessionwire.TenantID]*harnessstore.Store) (department.LaunchTarget, error) {
	launch := func(ctx context.Context, binding Binding) (harnessruntime.Launcher, error) {
		journal, ok := journals[binding.Tenant]
		if !ok {
			return nil, &UnknownTenantError{Tenant: binding.Tenant}
		}
		binding.Journal = journal
		assembled, err := agent.Define(ctx, binding)
		if err != nil {
			return nil, err
		}
		if assembled == nil {
			return nil, &NoRigError{Agent: agent.ID}
		}
		return assembled, nil
	}
	rigs := harnessruntime.RigsFunc(
		func(ctx context.Context, request department.RigCreateRequest) (harnessruntime.Launcher, error) {
			return launch(ctx, Binding{Tenant: request.TenantID, Session: request.SessionID, WorkspaceRoot: request.WorkspaceRoot})
		},
		func(ctx context.Context, _ uuid.UUID, request department.RigRestoreRequest) (harnessruntime.Launcher, error) {
			return launch(ctx, Binding{Tenant: request.TenantID, Session: request.SessionID, WorkspaceRoot: request.WorkspaceRoot, Restore: true})
		},
	)
	var options []harnessruntime.Option
	if agent.Decode != nil {
		options = append(options, harnessruntime.WithBlockDecoder(agent.Decode))
	}
	return harnessruntime.Target(rigs, agent.Compatibility, agentCapabilities(agent), options...)
}

// UnknownTenantError is a launch or journal read for a tenant this stack does
// not serve.
type UnknownTenantError struct{ Tenant sessionwire.TenantID }

func (e *UnknownTenantError) Error() string {
	return "stack: tenant " + strconv.Quote(string(e.Tenant)) + " is not served by this stack"
}

// NoRigError is an Agent.Define that returned neither a rig nor an error.
type NoRigError struct{ Agent sessionwire.AgentID }

func (e *NoRigError) Error() string {
	return "stack: Agents[" + strconv.Quote(string(e.Agent)) + "].Define returned no rig and no error"
}

// openJournals opens every tenant's harness journal store.
func openJournals(s Storage, tenants []sessionwire.TenantID) (map[sessionwire.TenantID]*storage.Composite, map[sessionwire.TenantID]*harnessstore.Store, error) {
	backends := make(map[sessionwire.TenantID]*storage.Composite, len(tenants))
	journals := make(map[sessionwire.TenantID]*harnessstore.Store, len(tenants))
	for _, tenant := range tenants {
		backend, err := s.Journal(tenant)
		if err != nil {
			return nil, nil, fmt.Errorf("stack: open journal backend for tenant %q: %w", tenant, err)
		}
		if backend == nil {
			return nil, nil, fmt.Errorf("stack: Storage.Journal returned no backend for tenant %q", tenant)
		}
		journal, err := harnessstore.Open(backend, harnessstore.WithTenant(tenant))
		if err != nil {
			return nil, nil, fmt.Errorf("stack: open harness journal for tenant %q: %w", tenant, err)
		}
		backends[tenant] = backend
		journals[tenant] = journal
	}
	return backends, journals, nil
}

// ServeHost composes and starts one Host for a RemoteHosts deployment, over
// the same Storage, Tenants and Agents the Factory process uses. It returns
// the running Service and the handler to mount on the listener o.Base names
// (HostLink, /readyz, /healthz, /metrics).
//
// It does not own o.Storage. Shut down with the Service's Stop (which drains
// while HostLink is still served), then close the listener, then
// o.Storage.Close. On error nothing is left running, and o.Storage is still
// the caller's to close.
func ServeHost(ctx context.Context, o HostOptions) (*host.Service, http.Handler, error) {
	if err := validateHostOptions(o); err != nil {
		return nil, nil, err
	}
	_, journals, err := openJournals(o.Storage, o.Tenants)
	if err != nil {
		return nil, nil, err
	}
	generation := o.Generation
	if generation == 0 {
		if generation, err = nextGeneration(ctx, o.Storage.Control.KV, o.HostID); err != nil {
			return nil, nil, err
		}
	}
	service, err := composeHost(ctx, hostPlan{
		hostID: o.HostID, base: o.Base, generation: generation, capacity: o.Capacity,
		credential: o.Credential, live: o.Live, limits: o.Limits, logger: o.Logger,
		control: o.Storage.Control, workspaces: o.Storage.Workspaces, tenants: o.Tenants,
		journals: journals, agents: o.Agents,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := service.Start(ctx); err != nil {
		return nil, nil, errors.Join(err, service.CloseUnstarted(context.WithoutCancel(ctx)))
	}
	return service, service.Routes(), nil
}

// TokenVerifier is the host.AuthVerifier matching a static HostLink service
// token: the Host side of Options.Limits.HostLinkCredential. It compares in
// constant time and accepts the token for any tenant the Host serves
// (ServeHost refuses other tenants before asking it).
func TokenVerifier(token string) host.AuthVerifier { return tokenVerifier(token) }

type tokenVerifier string

func (t tokenVerifier) VerifyTenant(_ context.Context, _ sessionwire.TenantID, presented string) error {
	if len(t) == 0 || subtle.ConstantTimeCompare([]byte(presented), []byte(t)) != 1 {
		return errHostLinkRefused
	}
	return nil
}

var errHostLinkRefused = errors.New("stack: HostLink credential refused")

// tenantScopedVerifier refuses a HostLink connection for a tenant this Host
// does not serve before the product's verifier is asked.
type tenantScopedVerifier struct {
	tenants []sessionwire.TenantID
	inner   host.AuthVerifier
}

func (v tenantScopedVerifier) VerifyTenant(ctx context.Context, tenant sessionwire.TenantID, token string) error {
	for _, served := range v.tenants {
		if served == tenant {
			return v.inner.VerifyTenant(ctx, tenant, token)
		}
	}
	return &UnknownTenantError{Tenant: tenant}
}

// staticToken is the credential Factory presents on every HostLink dial.
type staticToken string

func (t staticToken) ServiceToken(context.Context) (string, error) { return string(t), nil }

// noCheckpoint commits nothing: stack v0.1.0 refuses agents that require a
// checkpoint, and the journal is the durable state.
type noCheckpoint struct{}

func (noCheckpoint) Checkpoint(context.Context, sessionwire.TenantID, sessionwire.SessionID) error {
	return nil
}

// localWorkspaces gives each session a directory under root. Release keeps
// it: the directory is the session's only copy of its files.
type localWorkspaces struct{ root string }

func (w localWorkspaces) EnsureWorkspace(_ context.Context, tenant sessionwire.TenantID, session sessionwire.SessionID) (string, error) {
	dir := filepath.Join(w.root, pathSegment(string(tenant)), pathSegment(string(session)))
	return dir, os.MkdirAll(dir, 0o700)
}

func (localWorkspaces) ReleaseWorkspace(context.Context, sessionwire.TenantID, sessionwire.SessionID) error {
	return nil
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// pathSegment maps a Core id (any UTF-8) to one safe path segment: itself
// when it is plainly safe, otherwise "~" and the SHA-256 of its bytes. "~" is
// outside the plain alphabet, so no id can climb out of the root or collide
// with a plain one.
func pathSegment(id string) string {
	if safeSegment.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "~" + hex.EncodeToString(sum[:])
}
