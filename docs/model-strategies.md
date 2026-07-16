2. Repository indexing beyond embeddings

Most agents still use vector search.

Better options:

Symbol graph

Nodes

* packages
* types
* methods
* variables
* interfaces

Edges

* calls
* implements
* imports
* embeds
* aliases
* references

This is what Sourcegraph, Kythe, SCIP, etc. build.

A 9B model reasoning over

Interface
↓
Implementation
↓
Caller
↓
Tests

is dramatically stronger than searching README chunks.

⸻

Dependency graph

Instead of asking

Find authentication

Agent asks

Who imports AuthService?
Who implements TokenVerifier?
Which packages depend on auth/jwt?

No embeddings required.

⸻

Workspace facts database

Example

Function Foo()
takes Context
returns error
calls:
  Parse
  Validate
  WriteCache

Now reasoning becomes graph traversal instead of token prediction.

⸻

3. Planning before editing

Small models jump straight into editing.

Better:

Observe
↓
Plan
↓
Locate
↓
Verify
↓
Edit
↓
Compile
↓
Repeat

Even a tiny planning phase helps.

⸻

Tree search variants

* DFS planning
* BFS planning
* Monte Carlo edits
* Beam search over edit sequences

Not common yet because they’re expensive.

5. Test-guided repair

DeepMind AlphaCode
Reflexion
SWE-Agent

Loop

Run tests
↓
Read failures
↓
Repair
↓
Run again

This is surprisingly robust.

⸻

6. Execution traces

Instead of code only, expose

Call stack
Variable values
Panic
Heap
Timing

A local model debugging from traces often beats one reading source.

⸻

8. Multiple candidate generation

Generate

5 edits
↓
Compile each
↓
Keep best

Or

3 plans
↓
Choose
↓
Implement

Works especially well with quantized models.

⸻

9. Retrieval of previous successful edits

Instead of documentation

retrieve

Last time this interface changed...
Last commit touching this API...
Previous bug fix...

Humans do this constantly.

⸻

10. Memory

Different levels

Short

Current task

Medium

Current workspace

Long

Developer preferences
Project conventions
Architecture

Claude Code’s memory is an example.

⸻

11. Explicit invariants

Instead of prose

Invariant:
Every API returns context.Context first.
No goroutines inside package X.
Never log secrets.
Always use slog.
Use errors.Join.

Small models obey checklists well.

⸻

12. Workspace contracts

Generate machine-readable summaries

Example

Package auth
Responsibilities:
Issue tokens
Validate JWT
Never touches database
Public API:
Issue()
Verify()
Refresh()

Much easier than summarizing in English.

⸻

14. Semantic compression

Instead of 40 files

compress into

Interfaces
↓
Call graph
↓
Types
↓
Comments
↓
Recent edits

Token budget drops enormously.

⸻

15. Incremental context

Don’t reload workspace.

Maintain

Current package
Open files
Dirty files
Affected graph

Only recompute changed nodes.

⸻

16. Confidence estimation

Model outputs

Confidence
Needed context
Potential risks
Unknown symbols

Agents become much better at asking for help.

⸻


18. Constrained decoding

Don’t let the model invent.

Grammar-constrained outputs

AST outputs

JSON

XML

S-expressions

Edit DSL

The parser rejects invalid outputs immediately.


⸻

Research worth reading

These are the papers and systems I’d prioritize:

* AlphaCode
* AlphaCode 2
* SWE-bench
* SWE-Agent
* OpenHands (formerly OpenDevin)
* Aider’s repository map
* Reflexion
* Self-Refine
* CRITIC
* Language Agent Tree Search (LATS)
* RepoCoder
* GraphCoder
* CodeR
* DSPy (optimization rather than coding, but relevant)
* Coccinelle (semantic patches)
* Kythe
* SCIP
* Tree-sitter
* LibCST
* Google’s internal Kythe/CodeSearch papers
* Sourcegraph Cody architecture posts

⸻

Where I think the next breakthrough is

Based on what you’ve described, I don’t think another prompting trick is where the big win lies.

The common pattern across successful systems is to progressively replace natural language with structured, deterministic representations. Models are excellent at reasoning but relatively weak at remembering and locating information. If your protocol lets a small model stop searching and instead navigate a semantic workspace, you’re reducing one of its biggest failure modes.

One idea that I haven’t seen explored deeply is a workspace transaction graph. Instead of emitting edits independently, every change becomes a node with explicit dependencies and verification criteria, for example:

Implement Interface X
    ↓
Create type Foo
    ↓
Implement methods
    ↓
Update factory
    ↓
Update tests
    ↓
Verify no unimplemented interfaces

The agent can only advance when each node’s verifier succeeds (compiler, LSP query, test, or static analysis). That shifts the problem from “generate correct code” to “satisfy a sequence of small, verifiable semantic goals.” For 9B–30B local models especially, that decomposition is likely to produce a larger improvement than simply using a bigger model.
