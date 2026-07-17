# Why we chose Go

The thesis: weak local models become effective repo-scale editors when raw
file editing is replaced by semantic queries and validated mutations with
typed rejections. Picking the target language came down to two questions.
Which language is easiest for a small model to reason about, and which
toolchain exposes enough semantic machinery to build the protocol without
writing a compiler?

Go won the first question. C# won the second. Go got the nod because the
agent's job has to be easy; ours only has to be possible.

## Why Go

Go removes most of the ambiguity that trips up local models:

* small grammar
* very little syntactic variation
* explicit imports
* explicit error flow
* no inheritance hierarchy
* limited metaprogramming
* structural interfaces
* standardized formatting
* fast compilation and tests
* predictable repository layout

That matters more than people give it credit for. A 7B or 14B model has
less language surface to understand and fewer plausible-but-wrong ways to
express a change.

More importantly, Go exposes most of the analysis stack as ordinary
libraries:

```
go/parser       syntax
go/ast          AST
go/types        type checking and symbol identity
go/packages     whole-program/package loading
go/ssa          SSA and call-oriented analysis
go/analysis     modular analyzers
gopls           references, rename, fixes and refactorings
```

`go/packages` loads and type-checks complete programs. `go/ssa` provides an
analysis-oriented intermediate representation. gopls already performs
structured transformations: rename, extraction, inlining, code repair,
formatting. Nobody had to invent the semantic substrate; the work was
putting a better agent protocol over it.

## The shape we wanted

Not an MCP server with twenty vague tools. More like a small database
language:

```
inspect symbol "github.com/acme/store.(*Store).Put"
find callers of symbol_id="..."
find implementations of interface_id="..."
find writes to field_id="..."
find paths from handler_id="..." to effect="network"
```

And tightly scoped mutations:

```
rename_symbol
add_parameter
replace_call
implement_interface
extract_function
move_declaration
add_struct_field
wrap_error
add_test_case
```

Every mutation returns structured data:

```json
{
  "status": "rejected",
  "reason": "interface implementation would become incomplete",
  "affected_symbols": ["..."],
  "diagnostics": ["..."],
  "possible_repairs": ["add_method", "change_interface"]
}
```

That is where a smaller model wins. It chooses from bounded operations
instead of synthesizing arbitrary patches.

## What we passed on

### Rust

rust-analyzer is an excellent semantic engine: query-driven incremental
analysis, separate syntax and semantic representations. Philosophically it
is close to what we wanted.

The language itself asks a lot of a weak model:

* traits and associated types
* lifetime relationships
* coercions and autoderef
* macros
* feature-gated compilation
* conditional compilation
* complex generics
* borrow-checker-driven repairs

The compiler can tell an agent it is wrong, but choosing the right repair
often takes more reasoning than the equivalent Go. Rust could be made very
agent-friendly, and rust-analyzer is probably the right foundation for
whoever takes that on. For a proof of concept, too much of our time would
have gone to interfacing with compiler internals that were never designed
as a public surface, and to interpreting diagnostics written for a human
reader.

### C#

Roslyn is almost comically well suited to this kind of project. Its
workspace model already exposes:

* complete solutions and projects
* source text
* immutable syntax trees
* semantic models
* compilations
* symbol identities
* analyzers
* code fixes
* refactorings

The API is designed for programmatic inspection and transformation across
a whole solution, and a convincing demo would probably have landed here
fastest:

```csharp
var symbol = semanticModel.GetDeclaredSymbol(node);
var references = await SymbolFinder.FindReferencesAsync(symbol, solution);
```

The language is bigger though, with more history for a model to carry:

* overload resolution
* inheritance
* attributes
* LINQ
* delegates and events
* nullable-state analysis
* reflection
* source generators
* multiple equivalent syntactic styles

Roslyn optimizes for the builder. Go optimizes for the agent. We built for
the agent.

### C++

Clang may have the richest low-level code-query and transformation tooling
anywhere: LibTooling, AST matchers, refactoring APIs, compiler-grade
semantic information.

The language asks more of a small model than anything else we considered:

* macros obscure source identity
* templates carry enormous semantic complexity
* overload resolution is intricate
* undefined behavior is invisible to structural checks
* ownership is largely conventional
* build configurations alter the visible program
* small changes can surface distant failures

Building here would have proven the infrastructure impressive without
showing that the approach makes local agents useful.

### Zig

Appealing because the language is explicit, but the compiler APIs are not
ready to carry a project like this. Choosing it would mean living the same
lesson zero is working through: pick an interesting language, then
discover the semantic tooling is itself a research project.

## What we built

Ultimately, we chose Go for 
the target language and Go for the control plane, sitting directly 
on `go/packages`, `go/types`, `go/ssa`, and selected gopls machinery.

```
agent → semantic protocol → Go workspace model
                              ↓
                    transaction/refactoring engine
                              ↓
                 gofmt → go vet → go test → commit
```

The first operations:

- `inspect_symbol`
- `find_references`
- `rename_symbol`
- `change_signature`
- `implement_interface` 

Then the same local model runs in two modes, raw shell and source editing
against semantic operations only, on deliberately repository-wide tasks.
The bet: semantic mode shows a large improvement in completion rate,
particularly around missed callers, wrong symbols, imports, and
compile-repair loops. The bench in this repo measures exactly that.
