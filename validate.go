package stack

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/sessionstore"
	"github.com/looprig/storage"
)

// OptionError is every refusal Validate (and so Start and ServeHost) makes
// before any I/O. Field is the Options path, for example "Agents[1].Define".
type OptionError struct {
	Field  string
	Reason string
	Cause  error
}

func (e *OptionError) Error() string {
	message := "stack: " + e.Field + " " + e.Reason
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap returns the lower layer's refusal, if any.
func (e *OptionError) Unwrap() error { return e.Cause }

func refuse(field, reason string) error { return &OptionError{Field: field, Reason: reason} }

func refuseCause(field, reason string, cause error) error {
	return &OptionError{Field: field, Reason: reason, Cause: cause}
}

// Validate reports the first reason o may not run, without any I/O. Start
// calls it first; calling it yourself surfaces configuration errors before a
// process opens storage.
func Validate(o Options) error {
	if err := validateStorage(o.Storage); err != nil {
		return err
	}
	if err := validateTenants("Tenants", o.Tenants); err != nil {
		return err
	}
	if err := validateIdentity(o.Identity, o.Origins, o.AllowDevelopmentIdentity, o.Logger); err != nil {
		return err
	}
	if err := validateAgents(o.Agents); err != nil {
		return err
	}
	switch mode := o.Hosts.(type) {
	case nil:
		if err := validateInProcess(InProcess{}, o.Limits); err != nil {
			return err
		}
	case InProcess:
		if err := validateInProcess(mode, o.Limits); err != nil {
			return err
		}
	case RemoteHosts:
		if o.Storage.SingleProcess {
			return refuse("Hosts", "is RemoteHosts but Storage is single-process (stack/localdisk): a Host in another process cannot read journals on this machine's disk; use a shared backend")
		}
		if err := validateCredential("Limits.HostLinkCredential", o.Limits.HostLinkCredential); err != nil {
			return err
		}
	default:
		return refuse("Hosts", "is not a known mode; use stack.InProcess{} or stack.RemoteHosts{}")
	}
	if err := validateLive(o.Live); err != nil {
		return err
	}
	if err := validateHostLimits("Limits.Host", o.Limits.Host); err != nil {
		return err
	}
	return validateFactoryLimits(o.Limits)
}

// validateHostOptions is Validate for ServeHost.
func validateHostOptions(o HostOptions) error {
	if err := validateStorage(o.Storage); err != nil {
		return err
	}
	if err := validateTenants("Tenants", o.Tenants); err != nil {
		return err
	}
	if err := validateAgents(o.Agents); err != nil {
		return err
	}
	if err := o.HostID.Validate(); err != nil {
		return refuseCause("HostID", "is not a valid Core host id", err)
	}
	for i, tenant := range o.Tenants {
		if _, err := sessionwire.HostLinkEndpoint(o.Base, tenant); err != nil {
			return refuseCause("Base", "is not a bare HostLink base for Tenants["+strconv.Itoa(i)+"] (scheme and authority only, no path)", err)
		}
	}
	if o.Credential == nil {
		return refuse("Credential", "is required; stack.TokenVerifier(token) matches Options.Limits.HostLinkCredential")
	}
	if err := validateLive(o.Live); err != nil {
		return err
	}
	return validateHostLimits("Limits", o.Limits)
}

func validateStorage(s Storage) error {
	if s.Control == nil {
		return refuse("Storage.Control", "is required")
	}
	for _, primitive := range []struct {
		name    string
		present bool
	}{
		{"Ledger", s.Control.Ledger != nil},
		{"Leaser", s.Control.Leaser != nil},
		{"KV", s.Control.KV != nil},
		{"Blobs", s.Control.Blobs != nil},
		{"OrderedIndex", s.Control.OrderedIndex != nil},
	} {
		if !primitive.present {
			return refuse("Storage.Control."+primitive.name, "is required")
		}
	}
	lifecycle, ok := s.Control.Blobs.(storage.BlobReaderLifecycle)
	if !ok {
		return refuse("Storage.Control.Blobs", "does not implement storage.BlobReaderLifecycle, which SessionStore requires; a single-host filesystem store is adapted with (*storage.Composite).WithBoundedBlobReaders, which stack/localdisk does for you")
	}
	if bound := lifecycle.BlobReaderCloseBound(); bound <= 0 {
		return refuse("Storage.Control.Blobs", "declares a reader close bound of "+bound.String()+"; SessionStore requires a positive one")
	}
	if s.Journal == nil {
		return refuse("Storage.Journal", "is required")
	}
	if s.Workspaces == "" {
		return refuse("Storage.Workspaces", "is required")
	}
	if !filepath.IsAbs(s.Workspaces) {
		return refuse("Storage.Workspaces", "must be an absolute path")
	}
	return nil
}

// probeBase is the longest base an in-process Host advertises, used only to
// ask Core whether a tenant id is routable on HostLink (and fits once
// derived).
const probeBase sessionwire.InternalEndpoint = "ws://127.0.0.1:65535"

func validateTenants(field string, tenants []sessionwire.TenantID) error {
	if len(tenants) == 0 {
		return refuse(field, "must name at least one tenant")
	}
	seen := make(map[sessionwire.TenantID]int, len(tenants))
	for i, tenant := range tenants {
		at := field + "[" + strconv.Itoa(i) + "]"
		if err := tenant.Validate(); err != nil {
			return refuseCause(at, "is not a valid Core tenant id", err)
		}
		if _, err := sessionwire.HostLinkEndpoint(probeBase, tenant); err != nil {
			return refuseCause(at, "cannot be addressed on HostLink", err)
		}
		if first, dup := seen[tenant]; dup {
			return refuse(at, "repeats "+field+"["+strconv.Itoa(first)+"] ("+strconv.Quote(string(tenant))+")")
		}
		seen[tenant] = i
	}
	return nil
}

func validateIdentity(id Identity, origins []string, allowDevelopment bool, logger *slog.Logger) error {
	if id.Verifier == nil {
		return refuse("Identity.Verifier", "is required")
	}
	if id.Authorizer == nil {
		return refuse("Identity.Authorizer", "is required; factory.TenantAuthorizer{} is the stock tenant-boundary choice")
	}
	if len(id.CSRF.SharedKey) < identity.MinCSRFSharedKeyBytes {
		return refuse("Identity.CSRF.SharedKey", "is "+strconv.Itoa(len(id.CSRF.SharedKey))+" bytes; want at least "+strconv.Itoa(identity.MinCSRFSharedKeyBytes)+", the same bytes in every replica")
	}
	if id.CSRF.TokenTTL < 0 {
		return refuse("Identity.CSRF.TokenTTL", "must not be negative")
	}
	if len(id.CSRF.TrustedOrigins) != 0 {
		return refuse("Identity.CSRF.TrustedOrigins", "must be empty; Options.Origins is the one list of trusted origins")
	}
	if len(origins) == 0 {
		return refuse("Origins", "must name at least one trusted browser origin (scheme://host[:port])")
	}
	if err := csrfConfig(id, origins).Validate(); err != nil {
		return refuseCause("Origins", "are not usable trusted origins", err)
	}
	if id.DevelopmentOnly {
		if !allowDevelopment {
			return refuse("AllowDevelopmentIdentity", "must be set to run a development identity (stack/devauth); it is never enabled implicitly")
		}
		if logger == nil || !logger.Handler().Enabled(context.Background(), slog.LevelWarn) {
			return refuse("Logger", "must be set and enabled at WARN with a development identity, so its banner is never discarded")
		}
	}
	return nil
}

// csrfTokenTTL is the CSRF token lifetime when Identity names none.
const csrfTokenTTL = 12 * time.Hour

func csrfConfig(id Identity, origins []string) identity.CSRFConfig {
	config := id.CSRF.Clone()
	if config.TokenTTL == 0 {
		config.TokenTTL = csrfTokenTTL
	}
	config.TrustedOrigins = append([]string(nil), origins...)
	return config
}

func validateAgents(agents []Agent) error {
	if len(agents) == 0 {
		return refuse("Agents", "must name at least one agent")
	}
	seen := make(map[sessionwire.AgentID]int, len(agents))
	for i, agent := range agents {
		at := "Agents[" + strconv.Itoa(i) + "]"
		if err := agent.ID.Validate(); err != nil {
			return refuseCause(at+".ID", "is not a valid Core agent id", err)
		}
		if first, dup := seen[agent.ID]; dup {
			return refuse(at+".ID", "repeats Agents["+strconv.Itoa(first)+"].ID ("+strconv.Quote(string(agent.ID))+")")
		}
		seen[agent.ID] = i
		if err := agent.Compatibility.Validate(); err != nil {
			return refuseCause(at+".Compatibility", "is not a usable compatibility id", err)
		}
		if agent.Define == nil {
			return refuse(at+".Define", "is required")
		}
		if agent.Capabilities.RequiresCheckpoint {
			return refuse(at+".Capabilities.RequiresCheckpoint", "is not supported: stack v0.1.0 commits no checkpoints")
		}
		capabilities := agentCapabilities(agent)
		if !capabilities.PoolingPermitted() {
			return refuse(at+".Capabilities.CaptureSafety", "is "+strconv.Quote(string(agent.Capabilities.CaptureSafety))+", which does not permit pooled placement; declare department.CaptureSafetyStreaming (small or streamed tool output) or department.CaptureSafetyBoundedMaterialized (a finite limit)")
		}
		if err := capabilities.Validate(); err != nil {
			return refuseCause(at+".Capabilities", "are not valid", err)
		}
	}
	return nil
}

// agentCapabilities is what the stack declares for an agent: pooled, weight
// at least one, and full recovery (harnessruntime.Target forces it too).
func agentCapabilities(agent Agent) department.Capabilities {
	capabilities := agent.Capabilities
	capabilities.SupportsPooled = true
	if capabilities.AdmissionWeight == 0 {
		capabilities.AdmissionWeight = 1
	}
	capabilities.Recovery = department.Recovery{AttemptCloser: true, PersistenceFaults: true}
	return capabilities
}

func validateInProcess(mode InProcess, limits Limits) error {
	if limits.HostLinkCredential != "" {
		return refuse("Limits.HostLinkCredential", "must be empty under InProcess; the stack mints a random credential per process")
	}
	if mode.HostID != "" {
		if err := mode.HostID.Validate(); err != nil {
			return refuseCause("Hosts.HostID", "is not a valid Core host id", err)
		}
	}
	listen := mode.Listen
	if listen == "" {
		return nil
	}
	hostname, port, err := net.SplitHostPort(listen)
	if err != nil {
		return refuseCause("Hosts.Listen", "is not host:port", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return refuseCause("Hosts.Listen", "has no usable port", err)
	}
	if hostname != "localhost" {
		ip := net.ParseIP(hostname)
		if ip == nil || !ip.IsLoopback() {
			return refuse("Hosts.Listen", "must be a loopback address: HostLink carries no browser security and the in-process Host is never reachable from outside")
		}
	}
	return nil
}

func validateCredential(field, credential string) error {
	if len(credential) < MinHostLinkCredentialBytes {
		return refuse(field, "is "+strconv.Itoa(len(credential))+" bytes; want at least "+strconv.Itoa(MinHostLinkCredentialBytes))
	}
	return nil
}

func validateLive(live *host.LiveTextOptions) error {
	if live == nil {
		return nil
	}
	for _, value := range []struct {
		field    string
		negative bool
	}{
		{"Live.RateBytesPerSecond", live.RateBytesPerSecond < 0},
		{"Live.BurstBytes", live.BurstBytes < 0},
		{"Live.FlushInterval", live.FlushInterval < 0},
	} {
		if value.negative {
			return refuse(value.field, "must not be negative")
		}
	}
	// Host's HostLink transport rules (host/internal/realtime/hostlink),
	// restated so they refuse before any I/O: a burst must admit one maximum
	// frame, and burst plus thirty seconds of rate must fit a client queue.
	rate, burst := live.RateBytesPerSecond, live.BurstBytes
	if rate == 0 {
		rate = liveDefaultRateBytesPerSecond
	}
	if burst == 0 {
		burst = liveDefaultBurstBytes
	}
	if burst < liveMaxFrameBytes {
		return refuse("Live.BurstBytes", "is "+strconv.Itoa(burst)+"; it must admit one maximum "+strconv.Itoa(liveMaxFrameBytes)+"-byte frame")
	}
	if burst >= liveClientQueueBytes || rate > (liveClientQueueBytes-1-burst)/30 {
		field := "Live.RateBytesPerSecond"
		if live.RateBytesPerSecond == 0 {
			field = "Live.BurstBytes"
		}
		return refuse(field, "is too large: burst + rate × 30 seconds must stay below the 1 MiB client queue")
	}
	return nil
}

// Host's live-transport constants (host/internal/realtime/hostlink v0.16.0).
const (
	liveDefaultRateBytesPerSecond = 32 << 10
	liveDefaultBurstBytes         = 8 << 10
	liveMaxFrameBytes             = 4 << 10
	liveClientQueueBytes          = 1 << 20
)

func validateHostLimits(field string, raw HostLimits) error {
	for _, value := range []struct {
		name     string
		negative bool
	}{
		{"WarmTTL", raw.WarmTTL < 0},
		{"RegistryHeartbeat", raw.RegistryHeartbeat < 0},
		{"RegistryExpiry", raw.RegistryExpiry < 0},
		{"ClaimTTL", raw.ClaimTTL < 0},
		{"ApplyDeadline", raw.ApplyDeadline < 0},
		{"CommandQueueSize", raw.CommandQueueSize < 0},
		{"ReconcileInterval", raw.ReconcileInterval < 0},
		{"ReconcileBatch", raw.ReconcileBatch < 0},
		{"CompatibilityTimeout", raw.CompatibilityTimeout < 0},
		{"WorkPoll", raw.WorkPoll < 0},
		{"MaxCommandBodyBytes", raw.MaxCommandBodyBytes < 0},
		{"Link.PingInterval", raw.Link.PingInterval < 0},
		{"Link.PongTimeout", raw.Link.PongTimeout < 0},
		{"Link.MaxBindingsPerLink", raw.Link.MaxBindingsPerLink < 0},
		{"Link.MaxBindings", raw.Link.MaxBindings < 0},
		{"Link.MaxTenantLinks", raw.Link.MaxTenantLinks < 0},
		{"Drain.Grace", raw.Drain.Grace < 0},
		{"Drain.IdleBoundary", raw.Drain.IdleBoundary < 0},
		{"Drain.PublishBound", raw.Drain.PublishBound < 0},
	} {
		if value.negative {
			return refuse(field+"."+value.name, "must not be negative")
		}
	}
	if (raw.Link.PingInterval == 0) != (raw.Link.PongTimeout == 0) {
		return refuse(field+".Link.PongTimeout", "and Link.PingInterval must be set together")
	}
	if p := raw.Link.PingInterval; p > 0 && p < time.Second {
		return refuse(field+".Link.PingInterval", "must be at least one second (the wire carries whole seconds)")
	}
	if p, q := raw.Link.PingInterval, raw.Link.PongTimeout; p > 0 && q >= p {
		return refuse(field+".Link.PongTimeout", "must be shorter than Link.PingInterval")
	}
	l := raw.resolved()
	if l.RegistryExpiry < l.RegistryHeartbeat*host.MinHeartbeatsBeforeExpiry {
		return refuse(field+".RegistryExpiry", "is "+l.RegistryExpiry.String()+"; it must hold at least "+strconv.Itoa(host.MinHeartbeatsBeforeExpiry)+" RegistryHeartbeat intervals ("+l.RegistryHeartbeat.String()+" each)")
	}
	if l.ApplyDeadline < l.ClaimTTL*host.MinClaimAttemptsBeforeDeadline {
		return refuse(field+".ApplyDeadline", "is "+l.ApplyDeadline.String()+"; it must hold at least "+strconv.Itoa(host.MinClaimAttemptsBeforeDeadline)+" ClaimTTL lifetimes ("+l.ClaimTTL.String()+" each)")
	}
	if l.Drain.IdleBoundary > l.Drain.Grace {
		return refuse(field+".Drain.IdleBoundary", "is "+l.Drain.IdleBoundary.String()+"; it must not exceed Drain.Grace ("+l.Drain.Grace.String()+")")
	}
	if l.Drain.PublishBound > l.Drain.Grace {
		return refuse(field+".Drain.PublishBound", "is "+l.Drain.PublishBound.String()+"; it must not exceed Drain.Grace ("+l.Drain.Grace.String()+")")
	}
	if l.CommandQueueSize > host.MaxCommandQueueSize {
		return refuse(field+".CommandQueueSize", "exceeds host.MaxCommandQueueSize ("+strconv.Itoa(host.MaxCommandQueueSize)+")")
	}
	if l.ReconcileBatch > host.MaxReconcileBatch {
		return refuse(field+".ReconcileBatch", "exceeds host.MaxReconcileBatch ("+strconv.Itoa(host.MaxReconcileBatch)+")")
	}
	if b := l.MaxCommandBodyBytes; b != 0 && (b <= sessionstore.MaxInboxPayloadBytes || b > maxRuntimeBodyBytes) {
		return refuse(field+".MaxCommandBodyBytes", "must exceed SessionStore's 64 KiB inbox limit and not exceed Harness's 16 MiB runtime body limit")
	}
	return nil
}

// maxRuntimeBodyBytes is Harness's object-backed runtime body limit, which
// Host enforces on a referenced command body.
const maxRuntimeBodyBytes = 16 << 20

func validateFactoryLimits(l Limits) error {
	checks := []struct {
		field    string
		set      bool
		validate func() error
	}{
		{"Limits.Reconcile", l.Reconcile != nil, func() error { return l.Reconcile.Validate() }},
		{"Limits.ClientLink", l.ClientLink != nil, func() error { return l.ClientLink.Validate() }},
		{"Limits.HostLink", l.HostLink != nil, func() error { return l.HostLink.Validate() }},
		{"Limits.HTTP", l.HTTP != nil, func() error { return l.HTTP.Validate() }},
		{"Limits.Routes", l.Routes != nil, func() error { return l.Routes.Validate() }},
	}
	for _, check := range checks {
		if !check.set {
			continue
		}
		if err := check.validate(); err != nil {
			return refuseCause(check.field, "are not valid", err)
		}
	}
	return nil
}
