# Incorporate agent-tools and memory into Amux

## Objective

Move the twelve code, documentation, knowledge, and memory tools currently exposed by the `amux` MCP server into native Go packages. The desktop and headless workers will keep using the same `amux` MCP entry point. A clean Amux installation must run these tools with the `agent-tools` and `memory` executables absent. Amux already owns task tracking, agent identity, and gateway transport. Reuse those systems.

The compatibility baseline is the checksum-pinned `agent-tools` v2.0.0 and `memory` v1.13.11 release binaries in [bundled-tools.json](../packaging/bundled-tools.json). The [current MCP catalog](../cmd/amux/bundled_catalog.go) defines the public tool names, schemas, and argument handling. Confirm behavior against the pinned binaries, including where an older source checkout disagrees. Record applicable licenses and notices for any copied code, grammar, or model artifact.

## Compatibility and validation rules

Before starting **each milestone**, record a short preflight in its implementation PR. Include the concrete need, which code and tests will be reused, and why the proposed design is the simplest way to meet the compatibility target. Apply the milestone-specific YAGNI, DRY, and KISS checks below. Resolve any failed check before implementation.

Use actual process execution for acceptance. Use the release fetcher to stage the pinned tools in a temporary reference directory. Build the production `amux` binary and drive `initialize`, `tools/list`, and `tools/call` over MCP stdio. Capture reference output from the original binaries with the arguments produced by the current adapter. Where that adapter transforms the result, also run the current Amux MCP server as a reference. Compare the native MCP response with that public response.

Run both sides against equivalent isolated checkouts, stores, and initial state. Compare `isError`, content shape, text, ordering, and state changes. Require exact text where deterministic. A fixture may map nondeterministic IDs, temporary roots, and timestamps by field. For ranking, use fixed corpus, model, configuration, and query fixtures. Compare result IDs and order where deterministic, and document any supported platform-specific score tolerance. Keep real parser, index, SQLite, and inference execution in the acceptance path. Golden files and mock providers can supplement the reference execution.

Take a baseline of the existing tool catalog, including required and optional arguments, defaults, literal argument handling, output encoding, errors, and checkout scope. Reuse [stdio coverage](../cmd/amux/bundled_mcp_process_test.go), [pinned release contracts](../cmd/amux/bundled_version_contract_test.go), and [memory regression fixtures](../cmd/amux/bundled_memory_contract_test.go). Adapt them as tools move in-process. Exercise supported requests with absent or invalid paths, ambiguous results, empty matches, and cancellation wherever those cases apply.

## Milestone 1 — File reads and Markdown

**Preflight.** YAGNI limits the work to the three existing tools. DRY requires shared checkout resolution and Markdown heading handling. KISS favors direct file access and one Markdown parser with source positions.

| Tool | Native replacement and execution test |
|---|---|
| `code_read` | Read files with existing line-range semantics. Compare whole-file and ranged output on UTF-8 and CRLF fixtures, open-ended ranges, out-of-range requests, and large files. Verify absolute paths and symlink boundaries match the supported checkout policy. |
| `doc_outline` | Extract heading levels, labels, and source lines. Compare an outline containing nested, repeated, and Setext headings, fenced examples, and front matter. |
| `doc_section` | Find a heading and return the corresponding Markdown span. Compare nested-section boundaries, repeated headings, and a missing heading against the source command. |

## Milestone 2 — Symbols and project search

**Preflight.** YAGNI limits indexing and languages to behaviors exercised by the public tools. DRY requires one symbol extraction and indexing path for both lookup and search. KISS favors one parser integration and one index with explicit invalidation rules. Validate the parser choice on all supported build targets before adopting it.

| Tool | Native replacement and execution test |
|---|---|
| `code_symbols` | List symbol names, kinds, scopes, and positions. Compare pinned-source output for Go, Rust, Python, TypeScript, JavaScript, C/C++, and C# fixtures, including nested declarations and unsupported files. |
| `code_symbol` | Return a named declaration from a specified file or project index. Compare exact source slices, overloaded or ambiguous matches, and results after an edit or deletion. |
| `code_search` | Search project symbols and files with the existing query modes and limits. Compare result order, ignore rules, duplicate names, and index refresh behavior. The knowledge and mixed modes must also pass in Milestone 3 before cutover of this tool. |

## Milestone 3 — Knowledge lookup and graph

**Preflight.** YAGNI limits support to the resources, relations, and traversal modes reachable through the shipped MCP tools. DRY requires sharing indexed symbols and canonical resource identity with Milestone 2 and reusing existing OKF support where applicable. KISS favors indexed edges and bounded traversal using existing storage.

| Tool | Native replacement and execution test |
|---|---|
| `knowledge_get` | Resolve resource URIs, titles, and identifiers into compact cards. Compare text, metadata, links, and missing or ambiguous lookups against a seeded reference graph. |
| `knowledge_graph` | Traverse relationships with the existing relation and depth arguments. Compare node and edge order, depth boundaries, cycles, and missing resources. |
| `code_search` | Finish knowledge and mixed search modes. Compare cross-kind ranking, filters, and limits using the same seeded graph and indexed checkout. |

## Milestone 4 — Durable memory and legacy import

**Preflight.** YAGNI limits the local package to the shipped tools and the data needed for migration and retrieval. DRY requires reuse of project identity, memory validation, and SQLite conventions where their semantics match. KISS favors one transactional local store and an explicit, idempotent importer from a read-only snapshot.

| Tool | Native replacement and execution test |
|---|---|
| `memory_store` | Store content, memory type, tags, and scope. Run the source and native tools against isolated stores and compare returned metadata and persisted state, including duplicate and invalid requests. |
| `memory_get` | Resolve full IDs and prefixes and return ordered records. Compare full and partial batches, ambiguous or missing prefixes, and project and global records. |
| `memory_update` | Update existing content and optional tags while retaining identity and metadata. Compare retained fields, revision behavior, and errors, including the existing tombstone recovery contract. |

Import an actual pinned-format store and verify IDs, scopes, tags, timestamps, links, and deletion records against the original. Repeat import and retry after an interrupted migration. Verify the source database stays readable by the original tool and the native store contains one copy of each logical record.

## Milestone 5 — Memory search, recall, and synchronization

**Preflight.** YAGNI requires evidence from reference queries before adding ranking or model components. DRY requires one retrieval engine behind both MCP search and provider startup recall, with existing enrolled transport and memory push/pull semantics reused. KISS favors one supported inference path and the existing ranking and sync rules. Prove the model and runtime can ship without either helper executable before committing to packaging.

| Surface | Native replacement and execution test |
|---|---|
| `memory_search` | Preserve keyword and semantic retrieval, ranking, project/global scope, and limits. Compare real queries against the pinned tool using a fixed corpus that includes exact matches, semantic-only matches, competing scopes, and updates and deletions from Milestone 4. |
| Startup recall | Replace subprocess memory recall in MCP initialization and provider startup. Compare query scope, output limits, timeouts, failure behavior, and resulting provider guidance. |
| Memory sync | Exercise native push and pull over mTLS against disposable Rust and Go gateways. Compare the resulting records and revisions from equivalent isolated runs with the original memory tool. Include OKF round trips, duplicate retries, conflicting edits, and retained gateway deletions. |

## Milestone 6 — Cutover and release

**Preflight.** YAGNI keeps the agent-facing entry point at `amux` MCP. DRY requires desktop and headless workers to share the same native dispatch. KISS makes the native path the only production path once every preceding parity gate passes.

1. Run the full side-by-side execution matrix for all twelve tools. Keep pinned originals in test staging solely for compatibility checks.
2. Install the candidate package in clean macOS, Linux, and Windows environments. Confirm both helper binaries and any PATH or `AMUX_BUNDLED_DIR` overrides are absent. Verify MCP discovery and invocation, startup recall, migrated data, and sync.
3. Run an assigned task through the built desktop and a headless worker. Have the agent call code, documentation, and memory tools via `amux` MCP, then report the result through the task tools. Use both gateway implementations across these runs and verify the resulting transcript and task state.
4. Remove helper staging from development and release builds, the production subprocess runner, and helper-binary discovery once the clean-install gate passes. Update the release manifest and upgrade coverage for existing installations.
