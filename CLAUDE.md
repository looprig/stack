# CLAUDE.md — stack

`stack` is the supported single-process composition of Factory + Host +
harness rigs over one storage profile (tier 6). It exists because Factory may
not import Host or harness and Host may not import Factory, so the composition
cannot live in either.

## Boundaries

`import_boundary_test.go` is the authority; keep this list in step with it.

- No file imports `tools`, `llm`, `sandbox`, `controller`, or a product,
  UI or integration repository. The stack names no tool, provider or
  sandbox: those are the product's.
- Backends live behind subpackages: only `localdisk` imports `fsstore`, only
  `memory` imports `storage/memstore` (tests excepted). The root must never
  drag a storage technology into a consumer's graph.
- Name only published Looprig versions (`publishedPins`). No `replace`, no
  vendoring; a `GOWORK=off` failure means a release is owed upstream.

## Invariants

- `Validate` does no I/O and returns `*OptionError` with the exact Options
  path in `Field`. Every refusal has a table case, and key ones are
  mutation-checked.
- `Start` owns `Storage`: on any failure it has stopped what it started and
  closed storage; otherwise `Stop` closes it last. `factory.New` and
  `host.Compose` both run before anything is served.
- `Stop` order is fixed: Quiesce → Host Stop → Factory Stop → HostLink
  listener → stores → `Storage.Close`. `TestStopRunsTheSafeOrder` holds it.
- The journal binding, templates, evidence router and resolvers derive from
  one source. Do not add an option that lets two of them disagree.
- `devauth` stays loudly development-only: `Identity.DevelopmentOnly`
  requires `Options.AllowDevelopmentIdentity` and a WARN-enabled Logger.
  Examples never print a credential.

## Testing

- Red-green-refactor; mutation-test any guard that matters.
- `GOWORK=off GOTOOLCHAIN=go1.26.8 make check` before committing, plus
  `git diff --check`.
