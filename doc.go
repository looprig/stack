// Package stack is the supported single-process composition of Looprig:
// Factory (the public API and browser realtime link), a pooled Host running
// harness agents through host/harnessruntime, and one storage profile.
//
//	s, err := stack.Start(ctx, stack.Options{
//	    Storage:  store,                       // stack/localdisk, stack/memory, or shared
//	    Tenants:  []sessionwire.TenantID{"dev"},
//	    Identity: dev.Identity(),              // stack/devauth is DEVELOPMENT-ONLY
//	    Agents: []stack.Agent{{
//	        ID: "assistant", Compatibility: "my-app/assistant/v1",
//	        Capabilities: department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
//	        Define: defineRig, // rig.WithSessionStore(binding.Journal), ...
//	    }},
//	    Origins: []string{"http://localhost:8080"},
//	    Logger:  logger,                // enabled at WARN: devauth's banner
//	    AllowDevelopmentIdentity: true, // the explicit devauth opt-in
//	})
//	defer s.Stop(shutdownCtx)
//	http.ListenAndServe(addr, s.Handler())
//
// Factory still reaches the in-process Host over a loopback HostLink
// WebSocket, so the two talk only through Core records and SessionStore, as
// they would across machines. Moving the Host out is Hosts: RemoteHosts{} in
// the Factory process plus ServeHost in each Host process, over shared
// storage; Agents, Identity, UI and the rig code do not change.
//
// # What the stack fixes, so you cannot get it wrong
//
// Every one of these was a runtime failure a hand composition could make:
//
//   - The journal binding (JournalBindingID/JournalBindingVersion), the
//     launch template per agent, the Host's settlement evidence router and
//     Factory's journal resolver are derived from one source, so they cannot
//     disagree ("places nothing", 404, runtime_unavailable).
//   - Factory's pending-command placement is always on; the Host is pooled,
//     cross-tenant isolated, and advertises a BARE base.
//   - The HostLink credential is random per process under InProcess; the
//     Host generation is a durable counter in Storage.Control, not the clock.
//   - Every runtime is harnessruntime.Target: the six required capabilities,
//     AttemptCloser and PersistenceFaults (host's Durable profile), the
//     live-options seam (so Live.IncludeReasoning/IncludeToolSteps work),
//     principal and metadata copied onto every admitted command, a create's
//     first message re-presented, and an unresolved payload reference refused
//     (Host resolves referenced bodies before the attempt). Factory stamps the
//     verified principal (factory.WithPrincipalStamping).
//   - Stop runs the one safe order, on its own lifecycle (the caller's context
//     bounds only the wait): Factory Quiesce → Host Stop (drain while
//     HostLink is still served) → Factory Stop → HostLink listener → stores →
//     Storage.Close.
//
// # Obligations that remain yours
//
//   - SANDBOXED TOOLS ON LINUX: if any agent runs commands through
//     github.com/looprig/sandbox, call sandbox.Init() as the very first
//     statement of main. On Linux the sandbox re-executes the binary and
//     refuses to build an executor without it; the stack cannot do this for
//     you because it must run before anything else in the process.
//   - Pass Binding.Journal to rig.WithSessionStore in Agent.Define. A rig
//     journaling elsewhere is unreadable to Host and to Factory.
//   - Change Agent.Compatibility whenever a new build cannot replay an old
//     journal.
//   - Identity is yours. stack/devauth is for a developer's own machine only.
//
// Stack v0.1.0 deliberately has no object policy (Factory's object route
// answers 503), no checkpointer (an agent that RequiresCheckpoint is refused)
// and no dedicated placement; Kubernetes placement is the controller's.
package stack
