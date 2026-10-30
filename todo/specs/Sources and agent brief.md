# Sources and agent brief

Oct 15, 2026 · @Saidimu

Start here if you are implementing the workspace: clone the pinned sources, read the files mapped to each spec section, generate the TS fixtures, then follow the build order. The spec is in `Workspace specification.md` and the store contract is in `Store specification.md` (both in this folder).

## Pinned sources

Three repositories, one npm package and one toolchain are pinned, and each must be read at exactly these versions because go-anansi and hestia are alpha and change often.

| Source | Pinned at | Get it | Read it for |
| --- | --- | --- | --- |
| TS reference library `@asaidimu/utils-workspace` | 9.0.0, MIT | `bun add @asaidimu/utils-workspace@9.0.0`, then read `index.d.ts` (3,536 lines) and `index.mjs` (6,723 lines) in its package folder | Behaviour of the kernel, turn tree, content registry and prompt assembly. Its README promises features the code lacks (spec section 18). |
| go-anansi | v8.10.2, commit 9d0157d, 2026-10-03 | [github.com/asaidimu/go-anansi](https://github.com/asaidimu/go-anansi), module `github.com/asaidimu/go-anansi/v8` | The data layer: schemas, transactions, raw queries, codegen |
| hestia | Commit b5f159a, 2026-09-19 | [github.com/asaidimu/hestia](https://github.com/asaidimu/hestia) | The host for `workspace-hestia`. This commit requires go-anansi v8.10.1, one patch behind the pin above. |
| asaidimu/blobs | v1.3.4, commit fe21834, 2026-09-10 | [github.com/asaidimu/blobs](https://github.com/asaidimu/blobs) | Blob bytes. Read `PRODUCTION_READINESS.md` first; its July list of blockers needs re-checking against v1.3.4. |
| Go | 1.27, released 2026-08-19 | [Go 1.27 release notes](https://go.dev/blog/go1.27) | Required by go-anansi and hestia |

Published documentation:

- [go-anansi docs site](https://asaidimu.github.io/go-anansi/): the schema format, codegen and QueryBuilder are the stable parts. Query-engine internals and the event API are the most likely to change.
- [hestia core on pkg.go.dev](https://pkg.go.dev/github.com/asaidimu/hestia/core) and its [abstract package](https://pkg.go.dev/github.com/asaidimu/hestia@v1.15.2/core/abstract): the published docs are v1.15.2, slightly behind the pinned commit. The source is authoritative.

Things found in the sources that need an owner's decision:

- hestia's `LICENSE.md` is the full AGPL-3.0 text, while its README badge says MIT. go-anansi is AGPL with a commercial option.
- blobs' `LICENSE.md` is MIT text with unfilled template placeholders (`{{name}}`, `{{date}}`, `{{author}}`).
- go-anansi's `go.mod` still says `go 1.27rc1`, although Go 1.27 is released.

## Reading map

Read the TS package for behaviour and the Go repositories for contracts. In the TS table, `d.ts` is `index.d.ts` and `mjs` is `index.mjs` in the package folder, and the number is the line in the pinned 9.0.0 files. Go paths are relative to each repository root. "Spec" means the Workspace specification tab, and "Store" means the Store specification tab.

| Spec section | Read in the TS package |
| --- | --- |
| 6 Domain model | `d.ts:1772` Turn, `d.ts:1797` TurnNode, `d.ts:1805` Session, `d.ts:1405` Content, `mjs:3295` TurnTree |
| 7 Kernel | `d.ts:127` Command, `d.ts:176` Action, `d.ts:190` Middleware, `d.ts:357` Authorizer, `mjs:684` KernelCoordinator, `mjs:742` VirtualStateManager, `mjs:774` ExecutionEngine, `mjs:899` Kernel |
| 8 Content registry | `d.ts:1465` ContentDefinition, `mjs:2074` ContentRegistry, `mjs:2069` ToolCallRegex |
| 9 Prompt pipeline | `d.ts:2753` Prompt, `d.ts:2849` ContentRetriever (an interface with no implementation), `d.ts:3022` PromptAssembler, `mjs:4260` operating-system text, `mjs:4330` PromptAssembler, `mjs:3802` Session.snapshot |
| 10 Providers | `d.ts:2861` InferenceContext, `d.ts:2972` LLM, `mjs:4162` LLM. Search `mjs` for the Google adapter and executor. |
| 11 Agent loop and tools | `d.ts:1682` ToolDefinition, `d.ts:2951` TurnProcessor, `mjs:1516` AgentModule, `mjs:1559` tool:execute. There is no TS loop to read. |
| 12 Storage and blobs | `d.ts:1240` BlobStorage, `d.ts:1357` Blob, `mjs:945` WorkspaceDatabase, `mjs:2462` ContentModule, `mjs:2543` blob:register, `mjs:2572` blob:release, `mjs:4134` MemoryBlobStorage |
| 13 Public API | `mjs:4088` createWorkspace, `mjs:4893` Bootstrapper, `d.ts:3272` Bootstrapper |
| Store 7.3 to 7.5 History, fork, delete | `mjs:3033` SessionModule, `mjs:3084` session:fork, `mjs:3113` session:delete, `mjs:3119` turn:add, `mjs:3170` turn:edit, `mjs:3214` turn:branch, `mjs:3239` turn:delete, `mjs:3285` loadAllTurns, `mjs:3980` SessionManager |
| Store 7.9 Topics | `mjs:2643` createTopicMiddleware, `mjs:2701` getCurrentTopics, which returns at the first matching topic |

| Spec section | Read in the Go repositories |
| --- | --- |
| 7 Kernel | go-anansi `core/persistence/transaction/transaction.go` (commit and rollback hooks, nesting, opt-in retry) and `core/persistence/base/types.go` (Persistence and Transact) |
| 12 Storage and blobs | blobs: `README.md`, `PRODUCTION_READINESS.md`, `store/`, `streaming/`. go-anansi: `core/schema/definition/indexes.go` (composite, unique and partial indexes), `core/query/dsl.go` and `core/query/interactor.go` (raw queries), `core/query/native/types.go`, `sqlite/query/builder.go` |
| 14 Hosting | hestia: `README.md`, `core/abstract/dispatcher.go`, `core/abstract/message.go`, `core/runtime/chain.go`, `core/runtime/local-dispatcher.go`, `core/runtime/dispatch/dispatch.go` and `bind.go`, `core/runtime/context/identity.go`, `core/system/module.go`, `core/internal/boot/app.go` |
| 14 Streaming and disconnect | hestia: `core/interface/http/register.go` (how result channels are drained) and `core/interface/http/transport_fasthttp.go` (SSE). Look for the S-19 comment. |

| Spec section | Read for the data layer and DTOs |
| --- | --- |
| Store 9 anansi adapter | go-anansi docs, in `site/`: `guides/domain-modeling.md` (modeling rules, why not to use unions, a worked chat example), `reference/schema-format.md`, `reference/struct-tag-spec.md`, `tutorial/projections.md`, `tutorial/crud-with-models.md`. Code: `core/document/` (document, pool, binding), `core/persistence/collection/model_collection.go` (`ReadAs`, `CreateFrom`, `UpdateFrom`, cache), `core/persistence/collection/managed.go` (optimistic locking), `core/ephemeral/` (the in-memory backend), `core/query/interface.go` (`Capabilities`). |
| 14 DTOs | hestia: `core/runtime/dispatch/bind.go` (input binding by the `input` tag), and the schedules feature as the model to copy: `core/system/schedules/service.go`, `model/inputs.go`, `model/outputs.go`, `model/*.schema.json`, the generated `registrations.go`, and `sanitization.go`. |

| Spec section | Read for block validation, streams and authorization |
| --- | --- |
| 8 Block validation | anansi: `core/schema/definition/validator.go` (`NewDocumentValidator`, strict, partial and loose modes, predicates), `core/json/schema.go` (a JSON Schema compiler and validator), `core/common/error.go` (`Issue`), `core/sanitize/`, `codegen/typescript/`, `codegen/faker/`. Docs: `site/reference/schema-format.md` (constraints and predicates) and `site/explanations/schema-as-source-of-truth.md` (no JSON Schema translator ships). |
| 14 Streams and authorization | hestia: `core/runtime/dispatch/bind.go` (`HandleInputStream` and its drain contract), `core/abstract/dispatcher.go` (`Message.InputChannel`), `core/runtime/secure-dispatcher.go` (rule key, resource context, access check), `core/system/schedules/authorizer.go` (calling the permission manager and access controller from inside a feature), `core/abstract/policy.go` (`BindingPolicyStore`). |

## Local setup

Three small steps set up everything an agent reads or runs. The TS package was verified to load under Bun 1.4.3 and to expose 42 runtime exports.

First, create the Bun project and install the pinned package:

```bash
mkdir workspace-study && cd workspace-study
bun init -y
bun add @asaidimu/utils-workspace@9.0.0
```

Replace the generated `index.ts` with this smoke test, then run it with `bun run index.ts`:

```ts
import * as workspace from "@asaidimu/utils-workspace";

console.log(Object.keys(workspace).sort().join("\n"));
```

Bun warns about a peer dependency on `@asaidimu/utils-events` 2.0.1 and blocks two postinstall scripts. Neither stopped the package from loading. Bun also installs the `@google/genai` peer dependency, which the bundle imports at load time.

Then clone the Go sources at their pins and check the toolchain:

```bash
git clone https://github.com/asaidimu/go-anansi && git -C go-anansi checkout v8.10.2
git clone https://github.com/asaidimu/hestia && git -C hestia checkout b5f159a
git clone https://github.com/asaidimu/blobs && git -C blobs checkout v1.3.4
go version   # must report go1.27 or newer
```

These clones are read-only references until Phase 0 decides what the workspace imports. The new `workspace` repository is created in step 3 of the build order below.

## Build order

Seven steps take the work from fixtures to a hardened release. The phase gates are in section 17 of the Workspace specification.

1. **Fixtures, TS side.** In the Bun project, write scripts under `fixtures/` that drive the pure TS classes with hand-made data: `TurnTree` (`mjs:3295`, check its signature first, it reads turns through a repository with `loadAllTurns` and `loadHead`), `PromptAssembler` (`mjs:4330`) with hand-built prompts, and `ContentRegistry` parsing (`mjs:2074`). Write each case as JSON, with input and expected output, into `testdata/ts-fixtures/`. None of this needs a database.
2. **Spike, Go side.** Create the `workspace` repository on Go 1.27 with a `spike/` directory. Write one small program per open item in section 15 of the Store specification, plus hestia's client-disconnect behaviour, hestia's output sanitization and `asaidimu/blobs` v1.3.4. Record results in `docs/spike.md` and upstream tickets in `docs/upstream.md`. The exit is a go or no-go note.
3. **Scaffold.** Run `go mod init`, create the package skeleton from section 5 of the Workspace specification, add the import-boundary test and a Makefile with the targets from section 16, and start `workspacetest` with `FakeClock`, `SeqIDs` and the harness. Copy interfaces verbatim from the Store specification section 6 and the Workspace specification sections 7 to 13.
4. **Store first, tests first.** Write `storetest` (Store specification section 11), then build `store/memory` until it passes, then add property tests for the turn tree.
5. **Kernel on the memory store.** Follow section 7. Commands, units of work, serializer, authorizer, events.
6. **anansi store and blobs.** Build `store/anansistore` and the blob adapters, and make the differential test green.
7. **The rest, in order.** Registry, prompt pipeline, providers, agent loop, facade and reference CLI, the hestia adapter, then hardening, as in section 17.

## Working rules

These rules keep the implementation faithful to the spec and easy to review.

- **The spec is the contract.** Copy interfaces from the code blocks verbatim. If one must change, change the spec first and record why in `docs/adr/NNNN-title.md`.
- **Complete files only.** Never leave placeholder comments such as "rest unchanged". Every change delivers the whole module it touches.
- **Tests first for the store and kernel.** Write the contract or property test, then the memory implementation, then the anansi one.
- **Go style.** `context.Context` first, errors wrapped with `%w`, no package-level state, injected clock and IDs, and no `time.Sleep` in tests.
- **Import boundaries.** Never import hestia outside workspace-hestia, or anansi's persistence packages outside store/anansistore. A test enforces it.
- **Alpha dependencies.** Pin exact versions. When anansi or hestia lacks something, record it in `docs/upstream.md` and use the fallback the spec names, instead of patching around it inside the core.
- **Units.** Money in any example or test is integer micro-units of KES, and physical quantities are metric.
- **Disagreements.** When the TS behaviour and the spec differ, the spec wins. When the spec and reality differ, fix the spec in the same change.
- **Small commits.** Each phase gate is a CI job that must pass before the next phase starts.
