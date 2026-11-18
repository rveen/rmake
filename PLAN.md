# rmake — critical review of the spec and implementation plan

> Current status, decisions taken during implementation and the next steps
> are in `CLAUDE.md`. The spec is `README.md`.

Input: `README.md` (the `bld` handoff). New requirements: the tool is called
**rmake**, and it **must import Makefiles**. Guiding principle: *a simple
language that does not lose the power of make*.

---

## 1. Verdict on the README

The rebuild model (§4) is the strong part and should be kept almost as is:
content hashes, a key over command + inputs + discovered deps, automatic early
cutoff, crash-safe records, depfile ingestion, a single global graph. It fixes
make's real weaknesses (mtimes, recursive make, missing flag/command
dependencies) without importing Bazel's weight.

The language (§5) is the weak part. It is *small*, but several of make's
genuinely useful powers are lost, a few rules contradict each other, and some
semantics are under-specified in ways that will surface immediately when
translating real Makefiles. Also, "Compatibility with Makefile syntax" is listed
as a non-goal and the importer is an optional M7 item — that must change.

### 1.1 Power of make that the spec loses

| make feature | Spec today | Why it matters | Proposal |
|---|---|---|---|
| `$<` (first prerequisite) vs `$^` | only `$in` = all inputs | `foo.o: foo.c config.h` + `cc -c $<` is the most common rule shape in existence; with only `$in` you can't add a header dep without breaking the command | Ninja-style implicit inputs: `rule a.o from a.c \| config.h`. Items after `\|` are in the key but not in `$in` |
| Text functions (`patsubst`, `$(SRCS:.c=.o)`, `addprefix`, `notdir`, `filter`) | none; only rc-style distribution | Mapping sources → objects is the core idiom of every Makefile; the only route left is "glob the builddir", which is indirect and fails for `filter`-style selection | One built-in, no user functions: **substitution reference** `${srcs:src/%.c=$builddir/%.o}`. Covers patsubst, `:.c=.o`, addprefix/addsuffix, basename. Optionally `${x:dir}` / `${x:file}` later, only if import data shows need |
| `?=` and `ifdef` | `env NAME = default`; `if a == b` only; undefined var is an error | Overridable defaults are how every Makefile is configured; `ifdef DEBUG` is everywhere | `var x ?= default` (set unless given on CLI/env). `if defined x`. Fold `env` statement into `?=` (see 1.3) |
| Target-specific variables (`foo.o: CFLAGS += -O0`) | not expressible | Per-file flags are common (generated code, warnings off) | Allow `var` lines inside a rule block, scoped to that rule |
| Remaking included makefiles | not addressed | make rebuilds an included, generated makefile and restarts. The README itself says "configure = a generator that writes a Buildfile fragment" but gives no way to *build* that fragment | If an `include`d file is a rule output, build it first, then reload once |
| Chained pattern rules (`.y → .c → .o`) | eager instantiation from the first input glob | Works only if globbing builddir outputs is iterated to a fixpoint — not specified | Specify: instantiate pattern rules repeatedly until no new rules appear (bounded); overlapping instantiations of the same output → error naming both rules |
| Fallback order between pattern rules (`%.o: %.c` and `%.o: %.cc`) | duplicate output = error | Fine for hand-written files, but the importer needs a deterministic answer | Keep the error in the language; the importer resolves it statically (it knows which source exists) |
| Directory outputs | not addressed | Doc generators, `cargo doc`, etc. | Document the stamp-file idiom; don't add a feature |

### 1.2 Contradictions and gaps in the spec

1. **`if` vs. column 0.** §5.1 says a statement starts at column 0 and an
   indented line is an attribute; the §5.7 example indents `var exe` inside
   `if`. Pick one: statements inside `if … end` may be indented, and
   indentation means "attribute" only directly after a `rule`/`task` header.
2. **Empty-list concatenation is a trap.** With rc semantics, `-I$incs` with
   empty `incs` correctly yields *no words* — but the same rule makes
   `$builddir/app$exe` vanish entirely when `var exe =`. The §5.7 example is
   therefore broken on Linux. Define it explicitly: `var exe =` is the empty
   list, `var exe = ""` is one empty word; concatenation with the empty list
   yields the empty list. Fix the example to use `""`. The importer must emit
   `""` for make variables that are expanded inside a word.
3. **`env` means two different things**: a statement that reads the
   environment into a variable, and a rule attribute that passes a variable
   through to the child. Rename the attribute to `pass NAME…`; replace the
   statement by `?=` with a single precedence rule:
   *CLI `name=value` > environment > `?=` default*. Plain `=` is never
   overridden by the environment, only by the CLI. `+=` after a CLI override
   is ignored (as in make).
4. **When are variables in commands expanded?** Eager evaluation (§5.3) and
   `$in`/`$out` (defined per rule) conflict for `var compile = $cc -c $in`.
   Specify: `var` values are eager; `run`/`sh` lines are templates expanded at
   rule instantiation, so `$in` there is legal; `$in` in a top-level `var` is
   an error.
5. **Globs in `var`.** It is not stated whether `var srcs = src/*.c` globs.
   Proposal: words are never globbed implicitly; globs are expanded only in
   `from` lists and via an explicit `glob` keyword: `var srcs = glob src/**/*.c`.
   Implicit globbing is a well-known make/sh footgun.
6. **Ordering for key stability.** `$in` from a glob must be sorted; state it.
7. **Default target** when there is no `default` line: unspecified. Use the
   first rule/task, as make does (the importer relies on this).
8. **Task names vs files** must be unique across includes (one global
   namespace) — say so.
9. **`always` vs. the no-op goal.** `rule bin/server from` + `always` runs
   `go build` on every invocation, so the "< 100 ms no-op" goal cannot hold for
   such projects. State the goal as "no-op build with no `always` rules".
10. **Performance budget** for no-op: JSON-lines log parsing plus `**` walks
    plus stat of 10k files plus PATH lookup per rule. Achievable, but cache
    `LookPath` per argv[0], and keep the state store behind the interface so a
    binary format can replace JSON if profiling demands it.
11. **System headers from depfiles** (`/usr/include/...`) get hashed. Fine
    with the stat cache, but normalize depfile paths (absolute inside the
    root → root-relative) or keys will churn between machines/dirs.

### 1.3 Things to simplify or defer

- **Jobserver** (§6.2): valuable, but only once the importer delegates to
  `make` for un-importable parts. FIFO server in the delegation milestone; fd
  pairs and Windows semaphore last.
- **`sh` on Windows** (open question 2): require a POSIX `sh` in `PATH`
  (Git for Windows/busybox). Imported Makefile recipes are POSIX shell anyway,
  so mapping to `cmd`/PowerShell would never work for them.
- **Formatter** (open question 4) is no longer optional: the importer must
  *print* an rmake AST, so the printer is built anyway; `-t fmt` is free.
- **`.gitignore` in globs** (open question 3): no for v1; hidden dirs and
  builddir rules already cover the common case.

---

## 2. Makefile import: design

### 2.1 Two ways in, one engine

1. `rmake -t import [Makefile] > Rmakefile` — one-time conversion that produces
   a **readable** file plus a report of approximations (`# rmake: …` comments).
2. **Transparent mode**: running `rmake` in a directory with no `Rmakefile` but
   with a `Makefile` (or `include Makefile` from an Rmakefile) imports in memory
   and builds. If the import is not exact, refuse with the report and suggest
   `-t import`.

Do **not** give the core language make semantics (lazy `=`, `$(eval)`,
`$(call)`). The importer is a separate front-end that lowers make to the rmake
AST; the core stays small. This keeps "Compatibility with Makefile syntax" a
non-goal *for the language* while making it a goal *for the tool*.

### 2.2 Pipeline

```
Makefile ─▶ make lexer/parser ─▶ make evaluator ─▶ lowering ─▶ rmake AST ─▶ printer / eval
           (tabs, \ cont.,      (=, :=, ?=, +=,    (rules→rule,
            define, comments)    ifeq/ifdef,         .PHONY→task, …)
                                 include, functions)
```

The make evaluator keeps values **symbolic** where it can (so a simple
`CFLAGS = -O2 -Wall` becomes `var cflags = -O2 -Wall` and stays editable) and
falls back to concrete expansion when it must (computed names, `$(foreach)`,
`$(shell)`).

### 2.3 Translation table

| make | rmake |
|---|---|
| `X = …`, `X := …` | `var x = …` (recursive vars that reference later definitions are expanded at the point of use; warn if the order matters) |
| `X ?= …` | `var x ?= …` |
| `X += …` | `var x += …` |
| `ifeq/ifneq/ifdef/ifndef/else/endif` | `if … else … end`, or resolved statically when the condition depends only on make-internal values |
| `include f`, `-include f` | `include f`; `-include $(DEPS)` where DEPS are `.d` files and flags contain `-MD/-MMD` → `deps gcc` on the matching rules, the include is dropped |
| `a b: c d` + recipe | `rule a b from c d` (only for `&:` grouped targets; plain `a b:` = two independent rules) |
| recipe using `$<` only | first prereq explicit, rest after `\|` |
| recipe using `$^` | all explicit |
| `$@ $< $^ $* $?` | `$out`, `$in`, `$in`, `$stem`, `$in` (warn: rmake has no "newer than" set) |
| `%.o: %.c` | `rule %.o from %.c` |
| static pattern `$(OBJS): %.o: %.c` | concrete rules, or a pattern rule when OBJS is exactly a substitution of a glob |
| suffix rules `.c.o:` | pattern rule |
| `.PHONY: t` with recipe | `task t needs …` |
| order-only `\| dir` | dropped (rmake creates parent dirs) unless it names a real file target |
| dependency-only lines (`foo.o: foo.h`) | merged into the rule as implicit inputs |
| recipe lines | one `sh "…"` per line (make runs each line in its own shell); `.ONESHELL` → one `sh` |
| `@cmd` | `cmd` (rmake already prints only `desc`) |
| `-cmd` | `sh "cmd \|\| true"` with a warning |
| `$(MAKE) -C dir [t]` | `include dir/Makefile` (imported recursively, paths rewritten to root-relative); if that fails, a rule/task that runs `make -C dir` under the jobserver |
| `$(shell …)` | executed at import time **only with `--allow-shell`**, result inlined with a comment; otherwise an error with the location |
| `$(wildcard p)` | `glob p` |
| `patsubst`, `subst`-refs, `addprefix/addsuffix`, `basename` | substitution reference |
| `filter`, `filter-out`, `notdir`, `dir`, `sort`, `strip`, `foreach`, `call`, `eval` | evaluated concretely at import time |
| implicit built-in rules (`%.o: %.c` via `$(CC) $(CFLAGS) $(CPPFLAGS) -c`) | emulate GNU make's built-in C/C++/assembler/link rules and default variables (`CC=cc`, `CXX=g++`, …) — many small Makefiles depend on them |
| `::` double-colon rules, `vpath`, `.SECONDEXPANSION`, `.DELETE_ON_ERROR` etc. | v1: double-colon → error; `vpath` → resolved statically; the rest ignored with a note |

### 2.4 Verification

The importer is only trustworthy if checked against GNU make itself:

- **Differential test**: for each Makefile in a corpus, run `make -n -B
  <target>` and `rmake -n -B <target>` (via import) and compare the multisets of
  commands (normalised whitespace). Mismatch = test failure.
- Optional `rmake -t import --check` runs the same comparison for the user.
- Corpus in `testdata/makefiles/`: hand-written minimal cases per construct,
  plus a handful of real-world Makefiles (small C libraries, a LaTeX document,
  a code generator project, a recursive multi-dir project).

---

## 3. Revised language summary (deltas to README §5)

```
var    NAME (= | += | ?=) { word | "glob" pattern }
rule   outs from ins [ "|" implicit-ins ]
         var NAME (= | +=) …      # rule-local
         run … | sh "…" | deps gcc | depfile p | pass NAME… | always | desc "…"
task   NAME[(p, …)] [needs …]
         run … | sh "…" | pass NAME… | desc "…"
if     ( word (==|!=) word | defined NAME ) … [else …] end
include path        # may be generated by a rule → build, then reload once
default target…
```
Expansion: `$x`, `${x}`, `${x:pat=repl}`; rc distribution; empty list vs `""`
as in §1.2(2). Build file name: `Rmakefile` (fallback: import `Makefile`).

---

## 4. Go layout

As README §9, renamed, plus the importer:

```
cmd/rmake/main.go
internal/syntax      lexer, parser, AST, positions, printer (= formatter)
internal/eval        vars, ?=, substitution refs, if, includes, patterns (fixpoint) → graph
internal/glob        ** globbing, builddir rule
internal/graph       nodes, edges, implicit inputs, cycle detection, topo order
internal/hash        hashing + stat cache
internal/state       JSONL store behind an interface
internal/depfile     make-format depfile parser (shared with the importer's lexer rules)
internal/execx       spawn, env filtering, process groups, capture
internal/sched       scheduler, rebuild decision, -k, signals
internal/jobserver   FIFO (Unix), later fd-pair + Windows semaphore
internal/tools       why, query, graph, tasks, clean, targets, fmt, import
internal/makefile    make lexer, parser, evaluator, builtin rules, lowering → syntax AST
testdata/
```
Module path `github.com/rveen/rmake`, Go ≥ 1.23 (1.24 installed), stdlib only.

---

## 5. Milestones

Each ends with passing tests and a usable binary. The importer track (M6–M7)
depends only on M1, so it can start in parallel with M2–M5.

| # | Scope | Done when |
|---|---|---|
| **M0** ✅ | Fix the spec: apply §1.2 / §3 to README, rename `bld`→`rmake`, `Buildfile`→`Rmakefile`, move importer out of non-goals. `go mod init` | README is self-consistent; example in §5.7 is correct on all OSes |
| **M1** ✅ | Lexer, parser, AST **printer**, eval (incl. `?=`, `defined`, substitution refs, rule-local vars, `\|` implicit inputs, pattern fixpoint), graph, static checks; `-n`, `-t graph`, `-t targets`, `-t fmt` | Example loads; errors have file:line:col; parse→print→parse round-trips; parser fuzzed |
| **M2** ✅ | Serial execution, hashing + stat cache, state store, rebuild decision, `-t why/query/clean` | No-op second build; flag change rebuilds; deleted/modified output rebuilds; touch-only runs nothing |
| **M3** ✅ | Parallel scheduler, output capture, status line, `-k`, signals | Interrupt leaves no partial outputs; `-j1` ≡ `-j16` |
| **M4** | Depfiles, `always`, early cutoff, generated-include reload | Header change rebuilds only affected objects; identical output skips dependents; generated fragment is built then used |
| **M5** | Tasks with params, `needs`, CLI overrides & precedence, `-t tasks` | `rmake release 1.0` works; tasks never cached |
| **M6** | Importer I: make lexer/parser/evaluator, variables, conditionals, includes, explicit/pattern/suffix/static-pattern rules, automatic vars, `.PHONY`→task, `-include *.d`→`deps gcc`, GNU built-in rules, common functions; `-t import`, transparent `Makefile` mode | Differential tests pass for the construct corpus and ≥ 3 real single-directory Makefiles |
| **M7** | Importer II: recursive `$(MAKE) -C` → include, `--allow-shell`, delegation fallback + FIFO jobserver, `-t import --check` | A recursive multi-dir project imports to one graph; nested `make -j` under delegation respects the budget |
| M8 (deferred) | Windows (`sh` required), fd-pair & semaphore jobserver | Not scheduled: Windows is not an objective for now |
| M9 (opt.) | `--strict` access tracing (Linux), local CAS output cache, `tool` attribute hashing compiler binaries (open question 1) | — |

## 6. Risks

- **Make's long tail.** Real Makefiles use `$(eval $(call …))` templates,
  computed variable names and `$(shell)` heavily. Mitigation: concrete
  evaluation at import time + delegation fallback; measure coverage on the
  corpus rather than chasing full GNU make compatibility.
- **Readable vs. exact import.** Symbolic translation is readable but can
  diverge; concrete translation is exact but ugly. The differential test is
  the arbiter: prefer symbolic, fall back to concrete per variable when the
  comparison fails.
- **Language creep.** Every proposed addition (§3) must be justified by
  importer data or a README example. No user functions, no macros, ever.
