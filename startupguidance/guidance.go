// Package startupguidance builds the source-compatible physical startup context.
package startupguidance

import "path/filepath"

const (
	SessionGuidance = "[Amux session guidance] Use amux for task tracking. Associate substantive untracked work with a fitting task. Read task details/comments and platform requirements, then claim before starting; respect another execution's ownership. Read the proposed parent before creating scoped subtasks. Root exceptions use --root-reason user-directed or principle with a specification recording the explicit request or concrete core-principle finding, evidence and acceptance criteria; otherwise ask the user to create the root. Reviews and cross-project handoffs require fitting parents. Keep cross-project implementation in a delegated task under the source parent. Finish code with amux submit-work, listing every changed file; never commit or push task work directly or add attribution lines. Read normal task text; do not parse it with --json or scripts. Do not use native todo/plan tools; subagent and background tools remain available."
	CodeToolsHint   = " Explore with amux code_search, code_symbols and code_symbol before code_read; use doc_outline then doc_section for Markdown."
	MemoryToolsHint = " Recall with amux memory_search and memory_get; memory_store saves durable lessons and memory_update revises an inspected match. Search before adding; skip routine progress and source-recoverable facts."
)

func Context(codeTools, memoryTools bool, memories string) string {
	hints := SessionGuidance
	if codeTools {
		hints += CodeToolsHint
	}
	if memoryTools {
		hints += MemoryToolsHint
	}
	return hints + "\n" + memories
}

func MemoryQuery(project string) (string, bool) {
	base := filepath.Base(filepath.Clean(project))
	if base == "." || base == string(filepath.Separator) {
		return "", false
	}
	return base + " project decisions and conventions", true
}
