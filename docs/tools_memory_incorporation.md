# Incorporate agent-tools and memory into Amux

## Objective

Move the twelve code, documentation, knowledge, and memory tools currently exposed by the `amux` MCP server into native Go packages. The desktop and headless workers will keep using the same `amux` MCP entry point. A clean Amux installation must run these tools with the `agent-tools` and `memory` executables absent. Amux already owns task tracking, agent identity, and gateway transport. Reuse those systems.

The compatibility baseline is the checksum-pinned `agent-tools` v2.0.0 and `memory` v1.13.11 release binaries in [bundled-tools.json](../packaging/bundled-tools.json). The [current MCP catalog](../cmd/amux/bundled_catalog.go) defines the public tool names, schemas, and argument handling. Confirm behavior against the pinned binaries, including where an older source checkout disagrees. Record applicable licenses and notices for any copied code, grammar, or model artifact.

## Storage and execution boundaries

The Amux Go gateway database is the authoritative store for persistent tool data. Memory records and their search indexes, persisted code indexes, and any knowledge records or graph edges that require persistence belong in that database. Scope stored records to the gateway project and apply the existing authorization rules, including explicit access to global memory. Desktop, headless workers, and the standard CLI access persistent data through enrolled gateway APIs. Reuse [gateway memory storage and routes](../server/gateway_memories.go).

The checkout remains the source for file contents, Markdown, and file-backed OKF. Run format parsing, document lookup, and OKF graph traversal natively when the checkout provides everything needed for the correct result. Those operations should also work with the gateway disconnected. When a lookup needs persisted resources, edges, or indexes, use the gateway database. Any client-side scratch state is rebuildable. Existing client databases and indexes are migration sources.

| Tools | Data source |
|---|---|
| `code_read`, `doc_outline`, `doc_section`, `code_symbols` | Native reads and parsing against the selected checkout. |
| `code_symbol` | Native parsing for a specific file. Use the gateway index for project-wide lookup when persistence is required. |
| `code_search` | Use the gateway for persisted project indexes. Evaluate file-backed OKF natively where its data is sufficient, and verify mixed results against the reference. |
| `knowledge_get`, `knowledge_graph` | Use native OKF files and links first when they supply the full result. Read and write persisted knowledge records or relationships through the gateway where required. |
| `memory_store`, `memory_get`, `memory_update`, `memory_search` | Store and retrieve through the gateway memory service. Run persistent index maintenance and search against gateway-owned data. |

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

**Preflight.** YAGNI limits indexing and languages to behaviors exercised by the public tools. DRY requires one symbol extraction and indexing path for both lookup and search. KISS favors one parser integration and one gateway index with explicit invalidation rules. Index entries must identify the checkout content or revision they describe so separate checkouts cannot return each other’s stale positions. Validate the parser choice on all supported build targets before adopting it.

| Tool | Native replacement and execution test |
|---|---|
| `code_symbols` | List symbol names, kinds, scopes, and positions. Compare pinned-source output for Go, Rust, Python, TypeScript, JavaScript, C/C++, and C# fixtures, including nested declarations and unsupported files. |
| `code_symbol` | Return a named declaration from a specified file or project index. Compare exact source slices, overloaded or ambiguous matches, and results after an edit or deletion. |
| `code_search` | Search project symbols and files with the existing query modes and limits. Compare result order, ignore rules, duplicate names, and index refresh behavior. The knowledge and mixed modes must also pass in Milestone 3 before cutover of this tool. |

## Milestone 3 — Knowledge lookup and graph

**Preflight.** YAGNI limits support to the resources, relations, and traversal modes reachable through the shipped MCP tools. DRY requires sharing indexed symbols and canonical resource identity with Milestone 2 and reusing existing OKF support where applicable. KISS favors bounded traversal over native OKF files and gateway-backed edges where durable state is required. Prove the file-backed cases with the gateway disconnected.

| Tool | Native replacement and execution test |
|---|---|
| `knowledge_get` | Resolve resource URIs, titles, and identifiers into compact cards. Compare text, metadata, links, and missing or ambiguous lookups against a seeded reference graph. |
| `knowledge_graph` | Traverse relationships with the existing relation and depth arguments. Compare node and edge order, depth boundaries, cycles, and missing resources. |
| `code_search` | Finish knowledge and mixed search modes. Compare cross-kind ranking, filters, and limits using the same seeded graph and indexed checkout. |

## Milestone 4 — Gateway-backed memory tools

**Preflight.** YAGNI limits memory storage to the shipped tools and compatibility data. DRY requires reuse of gateway project identity, memory validation, and existing records and revisions. KISS favors one transactional server store shared by MCP and the CLI.

| Tool | Native replacement and execution test |
|---|---|
| `memory_store` | Store content, memory type, tags, and scope. Run the original tool against an isolated legacy store and the Go tool against a disposable gateway. Compare returned metadata and persisted state, including duplicate and invalid requests. |
| `memory_get` | Resolve full IDs and prefixes and return ordered records. Compare full and partial batches, ambiguous or missing prefixes, and project and global records. |
| `memory_update` | Update existing content and optional tags while retaining identity and metadata. Compare retained fields, revision behavior, and errors, including the existing tombstone recovery contract. |

Verify a fresh enrolled client on another machine can retrieve the same project memories and authorized global memories from the gateway.

## Milestone 5 — Gateway search, startup recall, and interoperability

**Preflight.** YAGNI requires evidence from reference queries before adding ranking or model components. DRY requires one retrieval engine behind both MCP search and provider startup recall, with existing enrolled transport and memory push/pull semantics reused. KISS favors one supported inference path and the existing ranking and sync rules. Prove the model and runtime can ship without either helper executable before committing to packaging.

| Surface | Native replacement and execution test |
|---|---|
| `memory_search` | Preserve keyword and semantic retrieval, ranking, project/global scope, and limits. Compare real queries against the pinned tool using a fixed corpus that includes exact matches, semantic-only matches, competing scopes, and updates and deletions from Milestone 4. |
| Startup recall | Replace subprocess memory recall in MCP initialization and provider startup. Compare query scope, output limits, timeouts, failure behavior, and resulting provider guidance. |
| Gateway interoperability | Exercise existing push and pull over mTLS with the pinned legacy client and disposable Rust and Go gateways. Verify the native tools can retrieve records after they arrive on the Go gateway. Compare records and revisions, including OKF round trips, duplicate retries, conflicting edits, and retained gateway deletions. |

## Milestone 6 — Migrate agent-memory data

**Preflight.** YAGNI limits the importer to supported source formats and data used by the shipped tools or required for compatibility. DRY requires reuse of gateway memory validation, canonical project mapping, and existing import or sync receipts. KISS favors a read-only source snapshot and resumable writes through the enrolled gateway.

- Add `amux --migrate memory` to the standard CLI. Resolve the existing agent-memory store and the configured Go gateway, with explicit source and gateway selection available for multiple installations. Use existing enrolled credentials. A missing source, ambiguous project mapping, or unsupported gateway capability must produce an actionable error.
- Import durable content, IDs or stable legacy ID mappings, project and global scopes, types, tags, timestamps, links, and deletion records. Preserve sync identities and revisions where compatible, and map them explicitly where gateway revisions are authoritative. Rebuild search data if the old index or embeddings are incompatible.
- Verify by executing the original tool against a seeded pinned-format store, running the migration command, then calling the native tools from a second enrolled client. Compare reads, search, and gateway records. Repeat after a partial failure and prove no duplicate records, silent conflict overwrites, or resurrected tombstones. Verify that the original store stays unchanged.

## Milestone 7 — Migrate agent-tools data

**Preflight.** YAGNI requires inventorying the legacy data and importing only durable records needed for the shipped tools. DRY requires reuse of the code indexer, native OKF parser, and migration reporting conventions from Milestone 6. KISS favors rebuilding derived indexes from the mapped checkout and importing only durable records that cannot be recovered from source.

- Add `amux --migrate tools` to the standard CLI. Discover the existing agent-tools data and associated checkout, or accept an explicit source. Resolve the gateway project before writing. Report imported, skipped, rebuilt, and conflicted records.
- Preserve durable resource IDs, metadata, and relationships required for parity. Keep source-backed OKF in its existing files when native reads reproduce the result. Import database-backed knowledge and any indispensable index metadata into the gateway database. Rebuild disposable symbol and search caches against the matching checkout and report data with no recoverable source.
- Verify by executing the pinned agent-tools against a real legacy fixture, running the migration command, and comparing symbol lookup, search, knowledge cards, and graph traversal from native MCP. Cover file-backed OKF with the gateway disconnected and durable knowledge from a clean enrolled client with no legacy cache. Repeat the migration and verify the source files and legacy store stay unchanged.

Both commands must report the selected source and gateway project before importing, summarize partial failures, and be safe to retry. The Go gateway must enforce authorization and reject ambiguous mappings and unauthorized global writes.

## Milestone 8 — Cutover and release

**Preflight.** YAGNI keeps the agent-facing entry point at `amux` MCP. DRY requires desktop and headless workers to share the same native dispatch. KISS makes the native path the only production path once every preceding parity gate passes.

1. Run the full side-by-side execution matrix for all twelve tools. Keep pinned originals in test staging solely for compatibility checks.
2. Install the candidate package in clean macOS, Linux, and Windows environments. Confirm both helper binaries and any PATH or `AMUX_BUNDLED_DIR` overrides are absent. Verify MCP discovery and invocation, startup recall, both migration switches, cross-client reads from the gateway, and native OKF access with the gateway disconnected.
3. Run an assigned task through the built desktop and a headless worker. Have the agent call code, documentation, and memory tools via `amux` MCP, then report the result through the task tools. Use the Go gateway for the full server-backed tool matrix. Exercise existing Rust gateway task compatibility where supported, and verify the resulting transcript and task state.
4. Remove helper staging from development and release builds, the production subprocess runner, and helper-binary discovery once the clean-install gate passes. Update the release manifest and upgrade coverage for existing installations.
