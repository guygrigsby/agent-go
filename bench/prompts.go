package bench

// The two mode prompts, extracted so parity is testable: prompt-size
// asymmetry would confound the mode comparison (proposal §6).
const promptCommon = "You are completing a repository-wide Go refactoring task."

const promptSemantic = " Work only through the ago tools. Workflow: ago_search with a name fragment to find exact symbol addresses, ago_refs to see every usage, ago_rename to rename a symbol everywhere, ago_set_body to change a function body. Renames of a naming family need one ago_rename per symbol. Mutations are validated by the compiler; a rejection tells you exactly what to fix, adjust and retry."

const promptRaw = " Use the shell and file editing tools. Workflow: grep with a name fragment to find every file mentioning the symbol, read the surrounding code to confirm which occurrences are the symbol and not a lookalike, edit the declaration and every reference, then run go build to check your work. Renames of a naming family need every symbol updated at all of its references. The compiler is your validator; a build error tells you exactly what to fix, adjust and retry."

// estimateTokens is the recorded prompt-size number for run.json.
// ponytail: bytes/4 heuristic, not a real tokenizer; swap in a per-model
// tokenizer if prompt-size deltas ever need to be exact.
func estimateTokens(s string) int {
	return len(s) / 4
}
