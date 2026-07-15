Go is probably the best target language for the actual experiment. C# is the best target if your priority is getting the semantic-editing layer working with the least compiler-tooling effort.

For your specific thesis—make weaker local agents effective by constraining them to semantic queries and validated transformations—I would choose Go.

Why Go fits weaker agents

Go already removes a lot of the ambiguity that makes local models stupid:

* small grammar
* very little syntactic variation
* explicit imports
* explicit error flow
* no inheritance hierarchy
* limited metaprogramming
* structural interfaces
* standardized formatting
* fast compilation and tests
* relatively predictable repository layout

That matters more than people give it credit for. A 7B or 14B model has less language surface to understand and fewer plausible-but-wrong ways to express a change.

More importantly, Go exposes most of the analysis stack as ordinary libraries:

go/parser       syntax
go/ast          AST
go/types        type checking and symbol identity
go/packages     whole-program/package loading
go/ssa          SSA and call-oriented analysis
go/analysis     modular analyzers
gopls           references, rename, fixes and refactorings

go/packages can load and type-check complete programs, while go/ssa provides an analysis-oriented intermediate representation. gopls already performs structured transformations including rename, extraction, inlining, code repair, formatting, and other refactorings.  

You would not be inventing the semantic substrate. You would mostly be putting a better agent protocol over it.

The shape I would build

Not an MCP server that exposes twenty vague tools. More like a small database language:

inspect symbol "github.com/acme/store.(*Store).Put"
find callers of symbol_id="..."
find implementations of interface_id="..."
find writes to field_id="..."
find paths from handler_id="..." to effect="network"

And then tightly scoped mutations:

rename_symbol
add_parameter
replace_call
implement_interface
extract_function
move_declaration
add_struct_field
wrap_error
add_test_case

Every mutation returns:

{
  "status": "rejected",
  "reason": "interface implementation would become incomplete",
  "affected_symbols": ["..."],
  "diagnostics": ["..."],
  "possible_repairs": ["add_method", "change_interface"]
}

That is where the smaller model wins. It chooses from bounded operations instead of synthesizing arbitrary patches.

Why not Rust first?

Rust has an excellent semantic engine in rust-analyzer, with query-driven incremental analysis and separate syntax and semantic representations. It is philosophically very close to what you want.  

But Rust itself is harder for a weak model:

* traits and associated types
* lifetime relationships
* coercions and autoderef
* macros
* feature-gated compilation
* conditional compilation
* complex generics
* borrow-checker-driven repairs

The compiler can tell the agent that it is wrong, but choosing the right repair often requires substantially more reasoning than equivalent Go code.

You could eventually make Rust extremely agent-friendly, but it is a harder proof of concept. You risk spending all your time wrapping unstable or internal compiler machinery and interpreting complicated diagnostics.

Why C# is the tooling winner

Roslyn is almost comically well suited to this project.

Its workspace model already exposes:

* complete solutions and projects
* source text
* immutable syntax trees
* semantic models
* compilations
* symbol identities
* analyzers
* code fixes
* refactorings

The API is specifically designed for programmatic inspection and transformation across an entire solution.  

You could probably reach a convincing demo in C# faster than in any other mainstream language:

var symbol = semanticModel.GetDeclaredSymbol(node);
var references = await SymbolFinder.FindReferencesAsync(symbol, solution);

Then emit a validated Solution transformation.

The downside is that C# is a larger language with more historical baggage:

* overload resolution
* inheritance
* attributes
* LINQ
* delegates and events
* nullable-state analysis
* reflection
* source generators
* multiple equivalent syntactic styles

Roslyn makes it easiest for you. Go makes the resulting system easiest for the agent.

C++ is the trap

Clang has perhaps the richest low-level code-query and transformation tooling: LibTooling, AST matchers, refactoring APIs, static analysis, and access to compiler-grade semantic information.  

But C++ is close to the worst possible language for a weak agent:

* macros distort source identity
* templates create enormous semantic complexity
* overload resolution is difficult
* undefined behavior is invisible to structural checks
* ownership is largely conventional
* build configurations alter the visible program
* small changes can trigger bizarre distant failures

You would prove that your infrastructure is impressive, not that the approach makes local agents useful.

My ranking

For building a useful local-agent coding environment:

1. Go
2. C#
3. Rust
4. Swift
5. C++
6. Zig

For building the fastest semantic-editing prototype:

1. C# / Roslyn
2. Go
3. Clang
4. Rust
5. Swift
6. Zig

Zig is appealing because the language is relatively explicit, but its compiler/tooling APIs are not mature enough for this purpose. You would repeat Zero’s mistake: selecting an interesting language and then discovering that the semantic tooling is itself the unfinished research project.

The strongest version

I would build it for Go, but write the control plane in Go as well and sit directly on go/packages, go/types, go/ssa, and selected gopls machinery.

Call it something like:

agent → semantic protocol → Go workspace model
                              ↓
                    transaction/refactoring engine
                              ↓
                 gofmt → go vet → go test → commit

Start with only five operations:

1. inspect_symbol
2. find_references
3. rename_symbol
4. change_signature
5. implement_interface

Then test the same local model in two modes:

* raw shell and source editing
* semantic operations only

Use deliberately repository-wide tasks. My expectation is that the semantic version would show a large improvement in completion rate, particularly around missed callers, wrong symbols, imports, and compile-repair loops.
