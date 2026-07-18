package bench

// The two mode prompts, extracted so parity is testable: prompt-size
// asymmetry would confound the mode comparison (proposal §6).
const promptCommon = "You are completing a repository-wide Go refactoring task."

const promptSemantic = " Work only through the ago tools. Workflow: ago_search with a name fragment to find exact symbol addresses, ago_refs to see every usage, ago_rename to rename a symbol everywhere, ago_set_body to change a function body, ago_add_param to add a parameter. sym arguments are symbol addresses like Type.Method, never file names. Mutations are validated by the compiler. When a rejection carries possible_repairs, resend the first repair's call exactly as given; never resend a rejected call unchanged."

// promptSerena mirrors promptSemantic's structure for the ablation arm:
// symbol addressing (LSP) with advisory validation. rename_symbol and
// replace_symbol_body edit through the language server, and
// get_diagnostics_for_file can report errors, but nothing blocks a bad
// write from landing; that gap is the variable under test.
const promptSerena = " Work only through the serena tools. Workflow: find_symbol with a name path to locate a symbol, find_referencing_symbols to see every usage, rename_symbol to rename it everywhere, replace_symbol_body to rewrite a declaration, replace_content for edits inside referencing files. Check your work with get_diagnostics_for_file on the files you changed; edits land whether or not they compile."

const promptRaw = " Use the shell and file editing tools. Workflow: grep with a name fragment to find every file mentioning the symbol, read the surrounding code to confirm which occurrences are the symbol and not a lookalike, edit the declaration and every reference, then run go build to check your work. Renames of a naming family need every symbol updated at all of its references. The compiler is your validator; a build error tells you exactly what to fix, adjust and retry."

// estimateTokens is the recorded prompt-size number for run.json.
// ponytail: bytes/4 heuristic, not a real tokenizer; swap in a per-model
// tokenizer if prompt-size deltas ever need to be exact.
func estimateTokens(s string) int {
	return len(s) / 4
}

// serenaRev pins the serena MCP server the bench runs; the frozen grid's
// third arm must be as reproducible as the model quants.
const serenaRev = "7c34662996f2d2e3db38001785e1f279dced2f50"
