package stack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/host"
	"github.com/looprig/sessionstore"
)

// Stack is one running composition: Factory, and under InProcess one pooled
// Host, over one Storage.
type Stack struct {
	factory *factory.Server
	host    *host.Service // nil under RemoteHosts

	hostStarted bool
	hostServer  *http.Server
	hostLn      net.Listener

	control  *sessionstore.Store                          // Factory's control store
	readers  map[sessionwire.TenantID]*sessionstore.Store // Factory's journal read side
	storage  Storage
	logger   *slog.Logger
	stopOnce sync.Once
	stopErr  error
}

// replicaID names this Factory in every claim it takes.
const replicaID = "stack"

// Start validates o, opens storage, composes Factory and (under InProcess)
// the Host, then starts them: Host first, so Factory's first placement finds
// it. Nothing is served until both compositions have succeeded; the only
// socket opened before host.Compose is the in-process Host's loopback
// listener, bound (not served) because the Host must advertise its address.
//
// START OWNS o.Storage. When Start returns an error, it has already stopped
// whatever it started and called o.Storage.Close; otherwise Stop closes it,
// last.
func Start(ctx context.Context, o Options) (_ *Stack, err error) {
	s := &Stack{storage: o.Storage, logger: o.Logger}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Stop(context.WithoutCancel(ctx)))
		}
	}()
	if err := Validate(o); err != nil {
		return nil, err
	}
	if o.Identity.DevelopmentOnly {
		s.logger.Warn("DEVELOPMENT IDENTITY: this process accepts development credentials and must never be exposed to anyone but its developer")
	}

	// SessionStore retains its Open context for its whole life; it must
	// outlive a cancelled start context, and Stop closes it explicitly.
	storeCtx := context.WithoutCancel(ctx)
	backends, journals, err := openJournals(o.Storage, o.Tenants)
	if err != nil {
		return nil, err
	}
	s.readers = make(map[sessionwire.TenantID]*sessionstore.Store, len(backends))
	for tenant, backend := range backends {
		reader, err := sessionstore.Open(storeCtx, backend, sessionstore.WithLegacySingleTenant(tenant))
		if err != nil {
			return nil, fmt.Errorf("stack: open journal reader for tenant %q: %w", tenant, err)
		}
		s.readers[tenant] = reader
	}
	if s.control, err = sessionstore.Open(storeCtx, o.Storage.Control); err != nil {
		return nil, fmt.Errorf("stack: open control store: %w", err)
	}

	credential := o.Limits.HostLinkCredential
	mode, inProcess := o.Hosts.(InProcess)
	if o.Hosts == nil {
		mode, inProcess = InProcess{}, true
	}
	if inProcess {
		if credential, err = randomToken(); err != nil {
			return nil, err
		}
	}
	if s.factory, err = s.composeFactory(o, credential); err != nil {
		return nil, err
	}

	if inProcess {
		if err := s.startInProcessHost(ctx, o, mode, credential, journals); err != nil {
			return nil, err
		}
	}
	if err := s.factory.Start(ctx); err != nil {
		return nil, fmt.Errorf("stack: start factory: %w", err)
	}
	return s, nil
}

func (s *Stack) startInProcessHost(ctx context.Context, o Options, mode InProcess, credential string, journals map[sessionwire.TenantID]*harnessstore.Store) error {
	listen := mode.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	hostID := mode.HostID
	if hostID == "" {
		hostID = "local"
	}
	generation, err := nextGeneration(ctx, o.Storage.Control.KV, hostID)
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	if s.hostLn, err = lc.Listen(ctx, "tcp", listen); err != nil {
		return fmt.Errorf("stack: bind the in-process HostLink listener: %w", err)
	}
	if s.host, err = composeHost(ctx, hostPlan{
		hostID: hostID,
		// A BARE base: Factory derives each tenant's HostLink address.
		base:       sessionwire.InternalEndpoint("ws://" + s.hostLn.Addr().String()),
		generation: generation, capacity: mode.Capacity,
		credential: TokenVerifier(credential), live: o.Live, limits: o.Limits.Host, logger: o.Logger,
		control: o.Storage.Control, workspaces: o.Storage.Workspaces, tenants: o.Tenants,
		journals: journals, agents: o.Agents,
	}); err != nil {
		return err
	}
	if err := s.host.Start(ctx); err != nil {
		return fmt.Errorf("stack: start host: %w", err)
	}
	s.hostStarted = true
	s.hostServer = &http.Server{Handler: s.host.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func(server *http.Server, ln net.Listener) { _ = server.Serve(ln) }(s.hostServer, s.hostLn)
	return nil
}

func (s *Stack) composeFactory(o Options, credential string) (*factory.Server, error) {
	service, err := identity.NewPrincipal(o.Tenants[0], "stack-factory", identity.KindService)
	if err != nil {
		return nil, fmt.Errorf("stack: service identity: %w", err)
	}
	directory, err := factory.NewStoreDirectory(s.control, factory.DefaultDirectoryLimits())
	if err != nil {
		return nil, err
	}
	templates := make([]factory.LaunchTemplate, 0, len(o.Agents))
	for _, agent := range o.Agents {
		templates = append(templates, factory.LaunchTemplate{Key: sessionstore.HostTargetKey{
			AgentID:                agent.ID,
			RuntimeCompatibilityID: string(agent.Compatibility),
			Placement:              sessionwire.HostPlacementPooled,
		}})
	}
	journals := host.NewPublicJournals(0)
	options := []factory.Option{
		factory.WithCredentialVerifier(o.Identity.Verifier),
		factory.WithAuthorizer(o.Identity.Authorizer),
		factory.WithCSRF(csrfConfig(o.Identity, o.Origins)),
		// Every Host a stack runs is harnessruntime-backed and advertises
		// principal attribution, so stamping the verified sender is safe.
		factory.WithPrincipalStamping(),
		factory.WithSessionReader(s.control),
		factory.WithCommands(s.control),
		factory.WithCatalog(s.control),
		factory.WithGates(s.control),
		factory.WithHostTargets(s.control),
		factory.WithDirectory(directory),
		factory.WithPublicCreates(s.control),
		// Without this nothing is ever placed on a Host.
		factory.WithPendingCommands(s.control),
		factory.WithDepartment(templates...),
		factory.WithSessionBinding(JournalBindingID, JournalBindingVersion),
		// A Host session's journal is the runtime's, not the control
		// store's; host's public projection keeps runtime ids from browsers.
		factory.WithSessionJournalResolver(func(_ context.Context, tenant sessionwire.TenantID, session sessionwire.SessionID, binding sessionstore.SessionBinding) (factory.JournalReader, error) {
			reader, err := s.journalReader(tenant, binding)
			if err != nil {
				return nil, err
			}
			return journals.Reader(reader, tenant, session, binding)
		}),
		factory.WithSessionObjectStoreResolver(func(_ context.Context, tenant sessionwire.TenantID, _ sessionwire.SessionID, binding sessionstore.SessionBinding) (factory.ObjectReader, error) {
			return s.journalReader(tenant, binding)
		}),
		factory.WithHostLinkCredential(staticToken(credential)),
		factory.WithServiceIdentity(service),
		factory.WithReplicaID(replicaID),
	}
	if o.Identity.CookieName != "" {
		options = append(options, factory.WithSessionCookieName(o.Identity.CookieName))
	}
	if len(o.Tenants) == 1 {
		options = append(options, factory.WithDefaultTenant(o.Tenants[0]))
	}
	if o.UI != nil {
		options = append(options, factory.WithUIHandler(o.UI))
	}
	if o.Logger != nil {
		options = append(options, factory.WithLogger(o.Logger))
	}
	if l := o.Limits.Reconcile; l != nil {
		options = append(options, factory.WithReconcileLimits(*l))
	}
	if l := o.Limits.ClientLink; l != nil {
		options = append(options, factory.WithClientLinkLimits(*l))
	}
	if l := o.Limits.HostLink; l != nil {
		options = append(options, factory.WithHostLinkLimits(*l))
	}
	if l := o.Limits.HTTP; l != nil {
		options = append(options, factory.WithHTTPLimits(*l))
	}
	if l := o.Limits.Routes; l != nil {
		options = append(options, factory.WithRouteLimits(*l))
	}
	server, err := factory.New(options...)
	if err != nil {
		return nil, fmt.Errorf("stack: compose factory: %w", err)
	}
	return server, nil
}

// UnknownJournalBindingError is a journal or object read for a session pinned
// to a binding this stack does not serve.
type UnknownJournalBindingError struct {
	Tenant    sessionwire.TenantID
	BindingID string
	Version   string
}

func (e *UnknownJournalBindingError) Error() string {
	return fmt.Sprintf("stack: unknown journal binding %q/%q for tenant %q", e.BindingID, e.Version, e.Tenant)
}

func (s *Stack) journalReader(tenant sessionwire.TenantID, binding sessionstore.SessionBinding) (*sessionstore.Store, error) {
	if binding.StorageBindingID != JournalBindingID || binding.BindingVersion != JournalBindingVersion {
		return nil, &UnknownJournalBindingError{Tenant: tenant, BindingID: binding.StorageBindingID, Version: binding.BindingVersion}
	}
	reader, ok := s.readers[tenant]
	if !ok {
		return nil, &UnknownTenantError{Tenant: tenant}
	}
	return reader, nil
}

// Handler serves /v1, ClientLink and the UI.
func (s *Stack) Handler() http.Handler { return s.factory.Handler() }

// Factory is the composed Factory.
func (s *Stack) Factory() *factory.Server { return s.factory }

// Host is the in-process Host, or nil under RemoteHosts.
func (s *Stack) Host() *host.Service { return s.host }

// Stop shuts the composition down in the one safe order and reports every
// failure: Factory Quiesce (fence new commands, close browser links) → Host
// Stop (drain while HostLink is still served) → Factory Stop → the HostLink
// listener → the stores the stack opened → Storage.Close, last. A drain that
// completed with failures is an error. Stop is idempotent; later calls return
// the first call's result.
func (s *Stack) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { s.stopErr = s.stop(ctx) })
	return s.stopErr
}

func (s *Stack) stop(ctx context.Context) error {
	var errs []error
	if s.factory != nil {
		errs = append(errs, s.factory.Quiesce(ctx))
	}
	if s.host != nil {
		if s.hostStarted {
			report, err := s.host.Stop(ctx)
			errs = append(errs, err)
			for _, failure := range report.Failures {
				errs = append(errs, fmt.Errorf("stack: host drain: %w", failure))
			}
		} else {
			errs = append(errs, s.host.CloseUnstarted(ctx))
		}
	}
	if s.factory != nil {
		errs = append(errs, s.factory.Stop(ctx))
	}
	if s.hostServer != nil {
		errs = append(errs, s.hostServer.Close())
	} else if s.hostLn != nil {
		errs = append(errs, s.hostLn.Close())
	}
	for _, reader := range s.readers {
		errs = append(errs, reader.Close(ctx))
	}
	if s.control != nil {
		errs = append(errs, s.control.Close(ctx))
	}
	if s.storage.Close != nil {
		errs = append(errs, s.storage.Close())
	}
	return errors.Join(errs...)
}

func randomToken() (string, error) {
	b := make([]byte, MinHostLinkCredentialBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
