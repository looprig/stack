# looprig/stack

`stack` is the supported single-process composition of Looprig: Factory (the
public API and browser realtime link), a pooled Host running harness agents
through `host/harnessruntime`, and one storage profile — started with one
call. It is what the browser-app starter's ~600 lines of `app.go` and
`runtime.go` did by hand, with every agreement those lines had to keep
derived from one place.

```sh
go get github.com/looprig/stack@latest
```

## Use

```go
dev, _ := devauth.New("dev", "")          // DEVELOPMENT-ONLY identity
store, _ := localdisk.Open("./data")      // fsstore roots; refuses a pre-v0.6.0 dir
s, err := stack.Start(ctx, stack.Options{
    Storage:  store,
    Tenants:  []sessionwire.TenantID{"dev"},
    Identity: dev.Identity(),
    Agents: []stack.Agent{{
        ID:            "assistant",
        Compatibility: "my-app/assistant/v1",
        Capabilities:  department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
        Define: func(ctx context.Context, b stack.Binding) (*rig.Rig, error) {
            return rig.Define(rig.WithLoops(agent), rig.WithPrimers("assistant"),
                rig.WithSessionStore(b.Journal)) // REQUIRED: journal where Host reads
        },
    }},
    Live:    &host.LiveTextOptions{IncludeReasoning: true, IncludeToolSteps: true},
    UI:      dev.Routes(myUI),
    Origins: []string{"http://localhost:8080"},
    Logger:  logger,                     // devauth needs one enabled at WARN
    AllowDevelopmentIdentity: true,      // the explicit opt-in devauth requires
})
// serve s.Handler(); on shutdown: s.Stop(ctx) before closing your listener.
```

`examples/browser-app` is the complete program: its whole composition
(`app.go`) is under 60 lines, and its tests boot it, create a session over
Factory's REST API, send input to a scripted `inference/inferencetest` model,
read the journal, and restart over the same data directory to restore.

## API

| | |
|---|---|
| `Options{Storage, Tenants, Identity, Agents, Hosts, Live, UI, Origins, Logger, AllowDevelopmentIdentity, Limits}` | one composition |
| `Validate(Options) error` | every refusal that needs no I/O, as `*OptionError{Field, Reason, Cause}` |
| `Start(ctx, Options) (*Stack, error)` | validate → open storage → `factory.New` → `host.Compose` → start Host → start Factory. **Owns `Storage`**: closes it on failure, and `Stop` closes it last |
| `(*Stack).Handler / Factory / Host / Stop` | `Stop` runs Factory Quiesce → Host Stop (drain while HostLink is served) → Factory Stop → HostLink listener → stores → `Storage.Close`, on its own lifecycle: the caller's ctx bounds only the wait, storage closes only after every component has finished, and a retried Stop waits for the same teardown. A runtime awaiting a gate is abandoned crash-equivalently (gate preserved, journal lease released; `DrainReport().Abandoned`) so the next `Start` — even in the same process — restores it with the gate answerable; a `Parked` runtime is a leak and makes Stop return `*ParkedSessionsError` |
| `Agent{ID, Compatibility, Capabilities, Define, Decode}` | `Define(ctx, Binding)` builds the rig for every launch; `Binding{Tenant, Session, Journal, WorkspaceRoot, Restore}` |
| `InProcess{Listen, HostID, Capacity}` / `RemoteHosts{}` | where agents run |
| `ServeHost(ctx, HostOptions) (*host.Service, http.Handler, error)` | a Host process for `RemoteHosts`, over the same `Storage`, `Tenants` and `Agents`; an explicit `Generation` becomes the floor of the automatic counter |
| `TokenVerifier(token)` | the Host side of `Limits.HostLinkCredential` |
| `stack/localdisk` | `Open(dir)`: `<dir>/control`, `<dir>/journal/<tenant>`, `<dir>/workspaces`; bounded blob readers; `*LegacyDataDirError` wraps `fsstore.ErrLegacyLayout` ("move or delete", never retried); single-process only |
| `stack/memory` | `New()` backend with shareable `Storage()` handles, `Open()` for one use; tests only |
| `stack/devauth` | **development-only** `"<user>:<token>"` identity, login form, per-process CSRF key; refused unless `Options.AllowDevelopmentIdentity` is set and `Options.Logger` is enabled at WARN; logs a banner on every start |

## What the stack fixes

The journal binding (`stack-journal`/`v1`), the per-agent launch templates,
Host's settlement evidence router and Factory's journal and object resolvers
are derived from one source. Placement is pooled and cross-tenant isolated;
Factory's pending-command placement is always on; the Host advertises a bare
base; the HostLink credential is random per process; the Host generation is a
durable counter in `Storage.Control` (`stack/host-generation/<sha256(HostID)>`),
not the clock. Every runtime is `harnessruntime.Target`, so recovery
(AttemptCloser, PersistenceFaults), live options, principal and metadata,
create re-presentation and payload-reference refusal hold by construction, and
Factory stamps the verified principal.

## Obligations that remain yours

- **Size the shutdown budget above `Drain.Grace` + 2 × `Drain.IdleBoundary`**
  (20 s at the defaults): both the context you pass to `Stop` and the
  platform's termination grace (`terminationGracePeriodSeconds`,
  `TimeoutStopSec`). A session waiting at a gate spends the whole grace
  having its release refused, then is abandoned under up to two idle
  boundaries. A shorter budget kills the process mid-abandon; a `Stop` whose
  context ends early returns and the teardown continues.

- **Sandboxed tools on Linux:** call `sandbox.Init()` as the very first
  statement of `main` if any agent runs commands through
  `github.com/looprig/sandbox`.
- Pass `Binding.Journal` to `rig.WithSessionStore`.
- Change `Agent.Compatibility` when a build cannot replay an old journal.
- Declare `Capabilities.CaptureSafety` honestly (streaming or bounded); set
  `RequiresWorkspace` if the agent needs `Binding.WorkspaceRoot`.
- Identity: `devauth` is for your own machine only.

## Moving Hosts out

Change `Hosts: stack.InProcess{}` to `stack.RemoteHosts{}`, set
`Limits.HostLinkCredential`, move `Storage` to a shared backend (natsstore, or
pgstore + s3store; `localdisk` is refused), and run `stack.ServeHost` in each
Host process with the same `Agents` and its own bare base. Stop order for the
split: Factory Quiesce → each Host's `Stop` → Factory Stop → storage.

## Not in v0.1.0

No object policy (Factory's object route answers 503), no checkpointer (an
agent that `RequiresCheckpoint` is refused), no dedicated placement
(Kubernetes placement is `controller`'s). Carbon
stays on its own composition until the stack reaches parity.

## Where it sits

Tier 6 composition library. Direct Looprig dependencies: `core`, `factory`,
`host` (including `host/harnessruntime`), `harness`, `sessionstore`,
`storage`, `inference` (examples and tests), and `fsstore` through
`stack/localdisk` only. It imports no tools, llm or sandbox;
`import_boundary_test.go` enforces that and that only `localdisk` imports
fsstore and only `memory` imports memstore.

## Verify

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 make check
```

Apache-2.0.
