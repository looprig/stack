package stack

import (
	"time"

	"github.com/looprig/host"
)

// Single-process Host defaults, used for every zero HostLimits field.
const (
	defaultWarmTTL              = 10 * time.Minute
	defaultRegistryHeartbeat    = 2 * time.Second
	defaultRegistryExpiry       = 10 * time.Second
	defaultClaimTTL             = 5 * time.Second
	defaultApplyDeadline        = 60 * time.Second
	defaultCommandQueueSize     = 16
	defaultReconcileInterval    = time.Second
	defaultReconcileBatch       = 32
	defaultCompatibilityTimeout = 20 * time.Second
	defaultWorkPoll             = time.Second
	defaultCapacity             = 16
)

func orDuration(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func orInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// resolved fills every zero field with its default.
func (l HostLimits) resolved() HostLimits {
	out := l
	out.WarmTTL = orDuration(l.WarmTTL, defaultWarmTTL)
	out.RegistryHeartbeat = orDuration(l.RegistryHeartbeat, defaultRegistryHeartbeat)
	out.RegistryExpiry = orDuration(l.RegistryExpiry, defaultRegistryExpiry)
	out.ClaimTTL = orDuration(l.ClaimTTL, defaultClaimTTL)
	out.ApplyDeadline = orDuration(l.ApplyDeadline, defaultApplyDeadline)
	out.CommandQueueSize = orInt(l.CommandQueueSize, defaultCommandQueueSize)
	out.ReconcileInterval = orDuration(l.ReconcileInterval, defaultReconcileInterval)
	out.ReconcileBatch = orInt(l.ReconcileBatch, defaultReconcileBatch)
	out.CompatibilityTimeout = orDuration(l.CompatibilityTimeout, defaultCompatibilityTimeout)
	out.WorkPoll = orDuration(l.WorkPoll, defaultWorkPoll)
	out.Link = host.LinkOptions{
		PingInterval:       l.Link.PingInterval,
		PongTimeout:        l.Link.PongTimeout,
		MaxBindingsPerLink: orInt(l.Link.MaxBindingsPerLink, 64),
		MaxBindings:        orInt(l.Link.MaxBindings, 256),
		MaxTenantLinks:     orInt(l.Link.MaxTenantLinks, 4),
	}
	out.Drain = host.DrainOptions{
		Grace:        orDuration(l.Drain.Grace, 10*time.Second),
		IdleBoundary: orDuration(l.Drain.IdleBoundary, 5*time.Second),
		PublishBound: orDuration(l.Drain.PublishBound, 2*time.Second),
	}
	return out
}
