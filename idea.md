# Why Go for agents

The thesis: weak local models become effective repo-scale editors when raw
file editing is replaced by semantic queries and validated mutations with
typed rejections. Picking the target language for that experiment comes down
to two questions. Which language is easiest for a small model to reason
about, and which toolchain exposes enough semantic machinery to build the
protocol without writing a compiler?

Go wins the first question. C# wins the second. Go is the pick because the
agent's job has to be easy; mine only has to be possible.

## Why Go fits weaker agents

Go already removes most of the ambiguity that makes local models look
stupid:

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
formatting. Nobody has to invent the semantic substrate; the work is
putting a better agent protocol over it.

## The shape

Not an MCP server that exposes twenty vague tools. More like a small
database language:

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

That is where the smaller model wins. It chooses from bounded operations
instead of synthesizing arbitrary patches.

## Why not Rust first

rust-analyzer is an excellent semantic engine: query-driven incremental
analysis, separate syntax and semantic representations. Philosophically it
is very close to what this needs.

Rust itself is the problem for a weak model:

* traits and associated types
* lifetime relationships
* coercions and autoderef
* macros
* feature-gated compilation
* conditional compilation
* complex generics
* borrow-checker-driven repairs

The compiler can tell the agent it is wrong, but choosing the right repair
often takes substantially more reasoning than the equivalent Go.

Rust could eventually be made very agent-friendly. It is a harder proof of
concept though; the risk is spending all the time wrapping unstable
compiler machinery and interpreting complicated diagnostics.

## Why C# is the tooling winner

Roslyn is almost comically well suited to this project. Its workspace model
already exposes:

* complete solutions and projects
* source text
* immutable syntax trees
* semantic models
* compilations
* symbol identities
* analyzers
* code fixes
* refactorings

The API is designed for programmatic inspection and transformation across a
whole solution. A convincing demo would probably land faster in C# than in
any other mainstream language:

```csharp
var symbol = semanticModel.GetDeclaredSymbol(node);
var references = await SymbolFinder.FindReferencesAsync(symbol, solution);
```

The downside is language size and historical baggage:

* overload resolution
* inheritance
* attributes
* LINQ
* delegates and events
* nullable-state analysis
* reflection
* source generators
* multiple equivalent syntactic styles

Roslyn makes it easiest for me. Go makes the resulting system easiest for
the agent. Optimize for the agent.

## C++ is the trap

Clang has maybe the richest low-level code-query and transformation
tooling: LibTooling, AST matchers, refactoring APIs, compiler-grade
semantic information.

C++ is also close to the worst possible language for a weak agent:

* macros distort source identity
* templates create enormous semantic complexity
* overload resolution is difficult
* undefined behavior is invisible to structural checks
* ownership is largely conventional
* build configurations alter the visible program
* small changes trigger bizarre distant failures

Building there proves the infrastructure is impressive, not that the
approach makes local agents useful.

## Ranking

For a useful local-agent coding environment:

1. Go
2. C#
3. Rust
4. Swift
5. C++
6. Zig

For the fastest semantic-editing prototype:

1. C# / Roslyn
2. Go
3. Clang
4. Rust
5. Swift
6. Zig

Zig is appealing because the language is explicit, but its compiler APIs
are not mature enough. That road repeats zero's mistake: pick an
interesting language, then discover the semantic tooling is itself the
unfinished research project.

## The strongest version

Build for Go, write the control plane in Go, and sit directly on
`go/packages`, `go/types`, `go/ssa`, and selected gopls machinery.

```
agent → semantic protocol → Go workspace model
                              ↓
                    transaction/refactoring engine
                              ↓
                 gofmt → go vet → go test → commit
```

Start with five operations:

1. inspect_symbol
2. find_references
3. rename_symbol
4. change_signature
5. implement_interface

Then run the same local model in two modes: raw shell and source editing,
and semantic operations only. Use deliberately repository-wide tasks.
Expectation: the semantic version shows a large improvement in completion
rate, particularly around missed callers, wrong symbols, imports, and
compile-repair loops.
