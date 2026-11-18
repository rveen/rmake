# rmake — specification

Build file name: `Rmakefile`. If there is no `Rmakefile` but there is a
`Makefile`, rmake imports the Makefile in memory and builds from it (§10).

This document specifies a language-neutral build tool written in Go. It
replaces make's rebuild model and syntax while keeping its core idea — a graph
of files built by commands, with only out-of-date parts rebuilt — and keeping
the expressive power that real Makefiles rely on. Existing Makefiles can be
imported.

## 1. Goals

- Single static binary, Go standard library only (allow `golang.org/x/sys` if needed).
- Language-neutral: works for C, C++, Go, Rust, code generators, documents.
- Correct incremental builds: content hashes, not timestamps.
- Small, declarative build language. No macros, no user-defined functions.
  Every language feature must be justified by a real use case or by Makefile
  import data.
- Import existing Makefiles: one-time conversion to a readable `Rmakefile`, or
  transparent use of a `Makefile` when the import is exact.
- Fast no-op build: < 100 ms for 10k files on a warm cache, for a build with no
  `always` rules.
- Explainable: the tool can always say why something ran.

## 2. Non-goals (v1)

- Hermetic/sandboxed builds (Bazel-style).
- Remote cache or remote execution.
- Built-in knowledge of any specific language or compiler (the Makefile
  importer's emulation of GNU make's built-in rules is the only exception, and
  lives in the importer, not the language).
- Make semantics in the rmake language itself (lazy variables, `$(eval)`,
  `$(call)`, user functions). Makefiles are *lowered* to rmake by the importer.
- Full GNU make compatibility. Constructs the importer cannot translate are
  reported, or delegated to `make` (§10.5).
- Configure/feature detection (use a generator rule whose output is included,
  §5.5).
- Windows. Target Linux and other Unix systems first. Keep Windows-specific
  parts (marked *(deferred)*) isolated behind build tags so they can be added
  later, but do not implement or test them now.

## 3. Lessons applied

| Source | Kept | Rejected |
|---|---|---|
| make | File DAG, rebuild only what is stale, `?=`, substitution references, pattern rules, target-specific variables, remaking included files | mtimes as truth, tab syntax, suffix rules, recursive make, `$@ $<`, `eval/call` |
| Ninja | Depfile ingestion, persistent build log, restat, implicit inputs (`\|`) | Hand-unwritable syntax |
| redo | Dependencies discovered at build time | One script per target |
| Shake / "Build systems à la carte" | Dynamic deps, early cutoff | Haskell DSL |
| Bazel | Rebuild key = hash of command + inputs | Hermeticity, size |
| tup | File-access tracing as a correctness check | Mandatory FUSE/tracing |
| rc | List variables, distributive concatenation | — |
| just / Task | Named tasks with arguments, separate from files | — |

## 4. Core semantics

### 4.1 Nodes

- **File rule**: outputs (1..n), explicit inputs (0..n), implicit inputs
  (0..n), rule-local variables, commands. Produces files.
- **Task**: name, optional parameters, dependencies (files or tasks), commands.
  Always runs when requested. Never cached. Replaces `.PHONY`.

Explicit and implicit inputs are identical for the graph and the rebuild key;
the only difference is that `$in` lists explicit inputs only.

### 4.2 Rebuild decision

For each file rule, after all its inputs are up to date, compute:

```
key = H( rmake_key_version,
         expanded command lines (argv form; sh lines as their string),
         resolved absolute path of each argv[0],
         values of variables listed in `pass` attributes,
         sorted (path, content_hash) of explicit and implicit inputs,
         sorted (path, content_hash) of discovered inputs from previous run )
```

Run the rule if any of:

1. No record exists for the rule.
2. `key` differs from the recorded key.
3. Any output is missing.
4. Any output's content hash differs from the recorded output hash (output modified externally).
5. The rule has `always`.

After success, record: key, output hashes, discovered inputs (from depfile), duration.

**Early cutoff is automatic**: dependents key on input *content* hashes, so if
a rebuilt output is byte-identical, dependents do not run. No special mechanism
needed. Test this explicitly.

Rule identity in the state store = hash of the sorted output paths.

`argv[0]` resolution through `PATH` is cached per name for the duration of a
run.

### 4.3 File hash cache

- Hash: SHA-256 (stdlib, hardware-accelerated on amd64/arm64).
- Cache entry per path: size, mtime (ns), inode (Unix) / file index (Windows), hash, time recorded.
- Reuse cached hash if size, mtime and inode match.
- Racy-clean rule (as in git): if the file's mtime is within 2 s of the time the entry was recorded, rehash anyway.

### 4.4 Discovered dependencies (depfiles)

- Attribute `deps make` (alias `deps gcc`): after the command succeeds, read the
  depfile (default `$out.d` for the first output, override with `depfile PATH`),
  record all prerequisites as discovered inputs, then delete the depfile.
- Format: make syntax. `target(s): prereq prereq \` with backslash-newline
  continuation, `\ ` for escaped spaces, `$$` for `$`. Multiple target lines
  allowed; union all prerequisites.
- Paths are normalized: cleaned, and absolute paths inside the project root are
  made root-relative, so keys do not depend on where the project is checked out.
- Missing discovered input on the next build means the rule must rerun (not an error).
  It is then recorded as missing, so the following build does not rerun it again.
- Prerequisites that are declared inputs or outputs of the rule are not
  recorded again; directories and other non-files count as missing.
- A depfile that the commands did not write means no discovered inputs (as in
  Ninja); a depfile that does not parse fails the rule. The depfile is also
  deleted when the rule fails or is interrupted.
- Discovered inputs order nothing: a *generated* header must also be declared
  as an input (`| gen/config.h`) so that it is built before the rules that
  include it.
- The depfile is read by the job that ran the commands; the discovered inputs
  are hashed after the commands finished, and the recorded key includes them.
- Sources that produce this format: gcc/clang `-MD -MF`, `rustc --emit=dep-info`,
  Cargo `.d` files, many generators.

### 4.5 Delegating to tools with their own cache

For `go build`, `cargo build` and similar, two options, both valid:

- Declare inputs with globs (`from go.mod go.sum **/*.go`).
- Mark the rule `always`. It runs every time; the tool's own cache makes that
  cheap; early cutoff stops dependents if the binary is unchanged. Such rules
  are excluded from the no-op performance goal.

### 4.6 Failure and interruption

- Before running a rule, create parent directories of its outputs. (This
  replaces make's order-only directory prerequisites.)
- On command failure: delete the rule's outputs, write no record.
- On SIGINT/SIGTERM: start no new jobs, forward the signal to all running
  process groups, wait, delete outputs of interrupted rules (and write no
  record for them), flush the state store, print `rmake: interrupted`, exit
  130. A process group still running 2 s after the signal (for instance a
  process that was being forked when it came) is killed with SIGKILL; a
  second SIGINT/SIGTERM kills all running groups at once. Rules that
  finished before the signal keep their outputs and records.
- After a command exits, rmake waits at most 10 s for its output to close (a
  background process left running by the command may hold it open); then the
  command fails.
- A crash between command and record write is safe: the next run sees no record
  or a hash mismatch and rebuilds.

### 4.7 Directory outputs

Outputs are files. A command that produces a directory tree (documentation
generators, `cargo doc`) declares a stamp file as its output and touches it as
the last command.

## 5. The build language

### 5.1 Lexical

- UTF-8. A `#` at the start of a word (outside quotes) starts a comment that
  runs to the end of the line; inside a word (`a#b`) it is literal.
- Lines are **headers** or **attributes**. The lines following a `rule` or
  `task` header that are indented **deeper than the header** (spaces or tabs,
  any amount; blank and comment lines in between are allowed) are its
  attributes. The first line indented no deeper than the header ends the
  block.
- Everywhere else, indentation is insignificant: statements inside
  `if … end` may be indented for readability (a rule inside an indented `if`
  indents its attributes one level further).
- Whitespace is space, tab, CR, VT and FF; CRLF line endings are accepted.
- Words are separated by whitespace. `"..."` makes one word containing spaces;
  `\"` and `\\` are escapes inside quotes. `""` is one empty word.
- `|` as a separate word in a `from` list separates explicit from implicit
  inputs. To use a literal `|`, quote it.
- Paths always use `/`, also on Windows.

### 5.2 Grammar

```
file       = { statement } .
statement  = var | include | default | rule | task | if .

var        = "var" NAME ( "=" | "+=" | "?=" ) value NL .
value      = { word } | "glob" pattern { pattern } .
include    = "include" path NL .
default    = "default" target { target } NL .
rule       = "rule" outpattern { outpattern } "from" { inpattern }
             [ "|" { inpattern } ] NL { rule_attr } .
task       = "task" NAME [ "(" NAME { "," NAME } ")" ]
             [ "needs" target { target } ] NL { task_attr } .
if         = "if" cond NL { statement } [ "else" NL { statement } ] "end" NL .
cond       = word ( "==" | "!=" ) word | "defined" NAME .

rule_attr  = INDENT ( "var" NAME ( "=" | "+=" ) value  # rule-local variable
                    | "run" { word }        # argv command, no shell
                    | "sh" word             # one quoted string, run by the shell
                    | "deps" ( "make" | "gcc" )
                    | "depfile" path
                    | "pass" NAME { NAME }  # pass these vars to the child env and include them in the key
                    | "always"
                    | "desc" word ) NL .
task_attr  = INDENT ( "var" NAME ( "=" | "+=" ) value
                    | "run" { word } | "sh" word | "pass" NAME { NAME } | "desc" word ) NL .
```

`from` with no inputs is allowed (`rule out/version.txt from` + `always`).

`if` cannot appear inside a rule or task block; use rule-local variables set
from top-level `if`s instead.

### 5.3 Variables and expansion

**Values.** All values are lists of words. `var x =` is the empty list;
`var x = ""` is a list of one empty word. These are different (see
concatenation).

**Definitions.**

- `var x = …` sets `x`. `var x += …` appends (to the empty list if `x` is
  undefined). Top-level definitions are evaluated eagerly, top to bottom.
- `var x ?= …` sets `x` only if it does not already have a value from the
  command line, the environment, or an earlier definition.
- Precedence: **command line** (`rmake x=v`) > **environment** (only for
  variables defined with `?=`) > definitions in the file. A command-line value
  makes every `=`, `+=` and `?=` of that name in the file a no-op.
- The environment is never read implicitly for `=`/`+=` variables.

**Globs.** Words in a `var` are never globbed. `var srcs = glob src/**/*.c`
expands the patterns (§5.4) into a sorted list. Globs under `$builddir` see
every declared output, including outputs declared further down (see
§5.4, *Evaluation to a fixed point*).

**Expansion.**

- `$name` or `${name}` expands a variable. `$$` is a literal `$`.
- A standalone `$x` expands to all its elements as separate words (separate
  argv entries).
- **Inside quotes** a reference expands to its elements joined by single
  spaces, so `sh "cc $cflags -c $in"` and `desc "link $in"` work as expected;
  `"$empty"` is one empty word.
- **Concatenation distributes** (rc semantics): if `x = a b`, then `-I$x` →
  `-Ia -Ib`. Concatenation with the empty list yields the empty list, so
  `-I$incs` with empty `incs` disappears. Concatenation with `""` yields the
  other side unchanged, so `app$exe` with `var exe = ""` is `app`.
  Concatenating two lists with more than one element each is an error.
- **Substitution reference** `${x:pat=repl}`: applied to each word of `x`.
  `pat` and `repl` may contain one `%`, matching any string (the stem). A word
  that does not match `pat` is kept unchanged. Without `%`, `pat` is matched as
  a suffix (make's `$(x:.c=.o)`). Examples, with `srcs = src/a.c src/b.c`:
  - `${srcs:src/%.c=$builddir/%.o}` → `build/a.o build/b.o`
  - `${srcs:.c=.o}` → `src/a.o src/b.o`
  - `${srcs:%=-I%}` → `-Isrc/a.c -Isrc/b.c`
- Undefined variable: error, with file:line:col. Use `?=` or `if defined`.
- `if a == b` compares both sides after expansion, each joined with spaces.
  `if defined x` is true if `x` has a value in scope or on the command line
  (the environment is not consulted).

**Built-ins.** `$os` (`linux`, `darwin`, `windows`, …), `$arch` (`amd64`,
`arm64`, …), `$builddir` (default `build`; may be reassigned only before the
first rule or include).

**Rule and task scope.**

- Inside rules: `$in` (explicit inputs, in declaration order; glob results
  sorted), `$out` (all outputs), `$stem` (the `%` match in pattern rules).
- Inside tasks: parameters are variables (`$version`), each one word: the
  argument, taken literally (never expanded). A parameter may not be named
  like a built-in (`os`, `arch`, `builddir`, `in`, `out`, `stem`), and
  parameter names must differ.
- Rule- and task-local `var` lines see top-level variables and `$in`, `$out`,
  `$stem`, and shadow top-level variables for that rule only. They are
  evaluated in order **before** the rule's other attributes, wherever they
  appear in the block.
- `run`/`sh` lines and rule-local vars are templates expanded when the rule is
  instantiated. Using `$in` or `$out` in a top-level `var`, or `$stem` outside
  a pattern rule, is an error.
- A task sees the variables as they were at the point of its definition
  (later redefinitions do not affect it); its commands are expanded when it is
  invoked with its arguments, and checked at load time with placeholder
  arguments.

### 5.4 Patterns and globs

- Globs: `*`, `?`, `[...]` within a path segment; `**` matches zero or more
  segments. Implemented in `internal/glob` (stdlib `filepath.Glob` lacks `**`).
- Glob resolution:
  - Paths under `$builddir`: match only outputs declared by rules (never what
    happens to be on disk). This prevents stale outputs from being picked up.
  - Other paths: match files on disk. Hidden directories (`.git`, `.rmake`) are skipped.
  - Results are sorted.
- **Pattern rules**: an output containing one `%` makes the rule a pattern
  rule. Every output contains exactly one `%`; the first explicit input
  contains exactly one `%` and no glob characters; other inputs contain at most
  one `%`. Instantiation: replace `%` with `*` in the first explicit input,
  glob it, and create one concrete rule per match with that stem. Because `*`
  does not cross `/`, the stem never contains a `/` (unlike make's `%`).
  Other inputs may contain globs, expanded per instance.
- Two rules (concrete or instantiated) producing the same output: error naming
  both (§5.8). There is no fallback order between pattern rules.
- **Evaluation to a fixed point.** Evaluation is a function of the set of
  declared outputs that globs under `$builddir` see. rmake evaluates the whole
  build file with the outputs declared by the previous evaluation (initially
  none) and repeats until that set no longer changes (at most 32 passes; a
  build that does not converge is an error). If no glob touched `$builddir`,
  one pass suffices. This makes declaration order irrelevant for globs and
  resolves chains of pattern rules (`build/%.c from src/%.y`, then
  `build/%.o from build/%.c`).

### 5.5 Includes and scoping

- `include sub/Rmakefile`. Paths in an included file (outputs, inputs, globs,
  nested includes, `default` and `needs` targets) are relative to that file's
  directory and rewritten to root-relative. Exceptions: absolute paths, and
  paths under `$builddir`, which is always root-relative (`$builddir/lib.a`
  in `sub/Rmakefile` is `build/lib.a`, not `sub/build/lib.a`). Command lines
  and `depfile` are never rewritten: commands run at the root.
- `include sub/Makefile` (any file named `Makefile`, `makefile`,
  `GNUmakefile` or ending in `.mk`) runs the importer on it (§10) and includes
  the result.
- Variables: an included file sees its parent's variables; its own definitions
  are local to it and files it includes.
- **Generated includes**: if an included path is the output of a rule
  (declared anywhere in the build), rmake builds it before using the graph:
  1. Load the build; a generated include that does not exist yet reads as
     empty. A missing include that no rule produces is an error.
  2. Build the generated includes (with the rules of that load; up to date
     ones run nothing). This happens for real even with `-n` and before
     `-t why/query/targets/tasks/graph` (as make remakes makefiles); with a
     tool the progress goes to stderr. `-t clean` builds nothing.
  3. If any of them changed, reload the whole build once.
  4. Generated includes that appear only in the reload (a fragment that
     includes another generated fragment) are built too; if one of them
     changes, that is an error (exit 2): one reload is all rmake does. The
     next run has both fragments and works.
- Commands always run with the project root as the working directory.
  `$in`/`$out` are root-relative.
- One global graph and one global namespace for tasks and targets. There is no
  recursive invocation.

### 5.6 Commands and environment

- `run` lines are argv lists executed directly (no shell). `argv[0]` is
  resolved through `PATH`.
- `sh "..."` runs `/bin/sh -c`; a script longer than 64 KB is fed to
  `/bin/sh -s` on stdin instead (Linux limits a single argument to 128 KB).
  Otherwise commands get no stdin. *(deferred)* On Windows, `sh` must be found
  in `PATH` (Git for Windows, busybox); otherwise using `sh` is an error.
- Several `run`/`sh` lines run in sequence; the first failure stops the rule.
- Child environment is filtered: `PATH`, `HOME`, `TMPDIR`, `TEMP`, `TMP`,
  `LANG`, `LC_*`, `TERM`, `SYSTEMROOT` (Windows), plus variables listed in
  `pass` attributes, plus jobserver variables (§7.2). Only `pass`-listed values
  enter the key (the rest cannot influence it except through the expanded
  command lines, which are keyed anyway).

### 5.7 Example

```
var cc ?= cc
var cflags ?= -O2 -Wall
var srcs = glob src/*.c

if $os == windows
  var exe = .exe
else
  var exe = ""
end

default $builddir/app$exe

rule $builddir/%.o from src/%.c | src/config.h
  desc "cc $stem"
  deps gcc
  run  $cc $cflags -MD -MF $out.d -c $in -o $out

rule $builddir/gen.o from src/gen.c
  var cflags += -Wno-unused
  run  $cc $cflags -c $in -o $out

rule $builddir/app$exe from ${srcs:src/%.c=$builddir/%.o}
  run  $cc -o $out $in

rule bin/server from
  always
  run  go build -o $out ./cmd/server

task test needs $builddir/app$exe bin/server
  desc "run all tests"
  run  ./$builddir/app$exe --selftest
  run  go test ./...

task release(version) needs test
  desc "tag release $version"
  run  git tag v$version
```

(`src/gen.c` matches both the pattern rule and the concrete rule for
`$builddir/gen.o`, which is the duplicate-output error of §5.8. In a real file,
narrow the pattern rule's input glob or give `gen.c` its own directory.)

### 5.8 Static checks (at load time)

- Two rules declaring the same output: error naming both.
- Cycle in the graph: error listing the cycle.
- Task and file target with the same name: error.
- Unknown or duplicate attribute, malformed `%`, `|` outside `from`,
  unterminated quote, `if` without `end`, a rule without commands, `depfile`
  without `deps`, assignment to `os`/`arch`: error with file:line:col.
- `$in`/`$out`/`$stem` outside a rule, undefined variable, list×list
  concatenation: error with file:line:col.
- An input that is neither a declared output nor an existing file: error when
  that rule is needed ("no rule to make X, needed by Y").

### 5.9 Default target

The targets of `default`. Without a `default` statement: the first output of
the first non-pattern rule, or the first task, in the root build file (as in
make, pattern rules never provide the default).

## 6. Formatting

`rmake -t fmt [file]` prints a build file in canonical form (two-space
attribute indent, aligned attribute values within a block, one blank line
between blocks, comments preserved). The same printer produces the output of
`-t import`. Parse → print → parse must round-trip.

## 7. Execution

### 7.1 Scheduler

- Topological, parallel. Default `-j` = `runtime.NumCPU()`; values below 1
  mean 1.
- A step (rule or task) becomes ready when every step producing its inputs
  (or a task it needs) has finished. A ready rule is decided at once (key
  computed; run or skip), without waiting for a job slot, so up-to-date
  rules never queue behind running jobs. Steps decided to run wait for a
  slot; among them the earliest in plan order starts first, so `-j1` runs
  in plan order.
- Decisions, hash-cache updates, state writes and output all happen on one
  scheduler goroutine; workers only run commands, capture output and hash
  the rule's outputs. Early cutoff follows: a dependent is decided only after
  its inputs have been rebuilt and rehashed.
- `-k`: keep going after failures (build everything not depending on a failed
  rule). Without `-k`: start no new jobs after the first failure, wait for
  running jobs (and record those that succeed), exit 1. Rules skipped because
  an input failed print nothing.
- Each command runs in its own process group (Unix; *(deferred)* job object on
  Windows) for clean signal delivery.

### 7.2 Jobserver

- rmake acts as a GNU make jobserver so nested `make`, `cargo`, `ninja` (with
  jobserver support) share the `-j` budget. This matters most for Makefile
  delegation (§10.5).
- Unix: create a FIFO in `.rmake/`, pre-fill with `j-1` tokens, export
  `MAKEFLAGS=" -j<j> --jobserver-auth=fifo:<path>"`. rmake's own scheduler
  draws from the same tokens.
- Older clients using `--jobserver-auth=R,W` fd pairs: via `exec.Cmd.ExtraFiles` (M8, deferred).
- *(deferred)* Windows: named semaphore (`--jobserver-auth=<name>`) (M8).

### 7.3 Output

- Capture stdout and stderr per job (combined, in order), print on completion
  as one block with a header (`desc` or the command). A block always ends
  with a newline.
- The total counts the planned steps not yet known to be up to date: steps
  decided up to date (or skipped after a failure) leave it, so after the build
  it equals the number of steps that ran.
- On a TTY (stdout is a character device and `TERM` is not `dumb`): a single
  status line `[done/total] desc` (desc of the most recently started running
  job, cut to the terminal width) is overwritten in place. Only jobs with
  output print their `[n/total] desc` header and output, above the status
  line. The last status line stays on screen.
- Not on a TTY: one `[n/total] desc` line per job when it starts; its output
  follows when it finishes, preceded by the header again if other lines came
  in between.
- On failure: print the full command, exit code, and captured output.
- `-v`: print every command before running.

## 8. State store

- Directory `.rmake/` at project root (add to `.gitignore`). `.rmake/lock` is
  held (`flock`) while rmake runs, so concurrent runs in one project wait for
  each other.
- `.rmake/log`: append-only binary log behind the `state.Store` interface.
  (A JSON Lines log was measured first: decoding 40k records took 170 ms of a
  410 ms no-op build, so the format switched to binary as anticipated.)
  - A magic header, then records framed as `uvarint(len) payload crc32c`.
  - Record types: `file` (hash cache entry), `rule` (rule record, including
    the key's components so `-t why` can explain changes), `delrule`.
  - Strings are length-prefixed, maps are written with sorted keys.
- Load: read all records; last record per key wins. The log is valid up to
  the first incomplete or damaged record (torn write, bad checksum); the rest
  is discarded and cut off before the next append. Losing state is always
  safe: it only causes rebuilds.
- Write: buffer during the build, append and `fsync` at the end and on interrupt.
- Compact (rewrite to a temp file, `fsync`, rename) when the log holds more
  than twice as many records as are live (and more than 1000).

## 9. CLI

```
rmake [flags] [name=value ...] [target|task [args...] ...]

flags:
  -C dir        change to dir first
  -f file       build file (default: Rmakefile, else Makefile via import)
  -j N          parallel jobs (make-style -jN also accepted)
  -k            keep going
  -n            dry run: list what would run, and why
  -B            treat every rule as out of date (with -n: list all commands)
  -v            verbose
  -t tool       run a tool instead of building (see below)

tools (-t):
  why TARGET     why TARGET would rebuild (or did last time): which input/flag/env changed
  query TARGET   rule, inputs, discovered inputs, outputs, command, last duration
  graph [T]      DOT graph of the whole build or of T's dependencies
  tasks          list tasks with desc and parameters
  targets        list all file targets
  clean          delete outputs recorded in state (never anything else)
  fmt [FILE]     print FILE in canonical form
  import [FILE]  convert a Makefile (default: Makefile) to Rmakefile syntax on stdout
                   --allow-shell   evaluate $(shell ...) at import time
                   --check         compare `make -n -B` with the imported build
```

Task arguments are positional: `rmake release 1.4.0`. A task's parameters
consume exactly that many following arguments, whatever they look like (an
argument `x=y` after a task is an argument, not an override). The progress
line shows the task's `desc` expanded with the arguments (`-t tasks` shows it
with `<param>` placeholders), else `task NAME ARGS`.

The same task with the same arguments runs once per invocation of rmake, even
if requested or needed several times. A task needed by another task may not
take parameters. Tasks are ordered only by `needs`: tasks and targets
requested together on the command line run in parallel under `-j`, as in
make. When `b` must run after `a`, declare `task b needs a` (or use `-j1`,
which runs requests in command-line order).

Tools use `-t` (as in Ninja) so tool names never collide with target or task names.

Exit codes: 0 ok, 1 build failure, 2 usage or build-file error, 130 interrupted.

Output: one `[n/N] desc` line per rule that runs (`desc`, else the first
command, shortened to 200 characters), followed by the command's captured
output (§7.3 for ordering under `-j` and the TTY status line). A failure prints `FAILED: outputs`, the command, its output and the
exit status. `-n` prints `# outputs: reasons` and the commands of every rule
that would run. When nothing runs: `rmake: no work to do`.

Diagnostics (environment variables): `RMAKE_TIMING=1` prints the duration of
each phase to stderr; `RMAKE_CPUPROFILE=file` writes a Go CPU profile.

## 10. Makefile import

### 10.1 Entry points

1. `rmake -t import [Makefile] > Rmakefile`: one-time conversion to a readable
   build file. Every approximation is marked with a `# rmake: …` comment and
   summarized on stderr.
2. **Transparent mode**: `rmake` without an `Rmakefile` but with a `Makefile`
   (or `include x.mk` from an Rmakefile) imports in memory and builds. If the
   import needs any approximation, rmake refuses, prints the report and
   suggests `-t import`.

The importer is a separate front end (`internal/makefile`). It produces the
same AST as the rmake parser; the core language gains no make semantics.

### 10.2 Pipeline

```
Makefile → make lexer/parser → make evaluator → lowering → rmake AST → printer | eval
```

- The lexer handles recipe tabs (and `.RECIPEPREFIX`), backslash continuation,
  `define … endef`, comments, and escapes.
- The evaluator implements `=`, `:=`, `::=`, `?=`, `+=`, `override`, `export`,
  `ifeq/ifneq/ifdef/ifndef`, `include/-include/sinclude`, and the make
  functions listed in §10.3.
- Values are kept **symbolic** where possible (`CFLAGS = -O2 -Wall` becomes
  `var cflags ?= -O2 -Wall`, still editable) and expanded **concretely** where
  necessary (computed variable names, `foreach`, `call`, `eval`, `shell`). The
  differential check (§10.6) decides: if the symbolic form diverges from make,
  that variable is expanded concretely.

### 10.3 Translation

| make | rmake |
|---|---|
| `X = …`, `X := …` | `var x = …`; recursive `=` variables referencing later definitions are expanded at their point of use, with a note |
| `X ?= …`, and variables make would take from the environment (`CC`, `CFLAGS`, …) | `var x ?= …` |
| `X += …` | `var x += …` |
| `override X = …` | `var x = …` with a note (rmake CLI values always win) |
| `ifeq/ifneq/ifdef/ifndef … else … endif` | `if … else … end` when it depends on overridable variables; resolved statically otherwise |
| `include f` | `include f` (imported recursively if it is a makefile) |
| `-include $(DEPS)` of `.d` files, with `-MD`/`-MMD` in the compile flags | `deps gcc` (+ `depfile` if not `$out.d`) on the matching rules; the include is dropped |
| `a: b c` + recipe | `rule a from b c` |
| `a b: c` + recipe | two rules (make semantics); `a b &: c` → one rule with two outputs |
| recipe uses only `$<` | first prerequisite explicit, the rest after `\|` |
| recipe uses `$^` / `$+` | all prerequisites explicit |
| recipe uses both `$<` and `$^` | concrete expansion of `$<` |
| `$@`, `$*`, `$?` | `$out`, `$stem`, `$in` (with a note: rmake has no "newer" set) |
| `$(@D)`, `$(@F)`, `$(<D)`, … | concrete expansion in instantiated rules |
| `%.o: %.c` | `rule %.o from %.c` |
| static pattern `$(OBJS): %.o: %.c` | pattern rule if OBJS is a substitution of a glob; else concrete rules |
| suffix rules `.c.o:` | pattern rule |
| several pattern rules for one target pattern | resolved statically per target, by which source exists, in make's order |
| GNU make built-in rules and variables (`%.o: %.c` via `$(COMPILE.c)`, linking, `CC=cc`, `CXX=g++`, `RM=rm -f`, …) | emitted as explicit rules/vars when the Makefile relies on them (C, C++, assembler, link, yacc, lex) |
| `.PHONY: t` | `task t needs …` |
| prerequisite-only lines (`foo.o: foo.h`) | merged into the rule as implicit inputs |
| order-only `\| dir` | dropped when it is a directory (rmake creates parents); else implicit input |
| recipe lines | one `sh "…"` per line; `.ONESHELL` → a single `sh` |
| `@cmd` | `cmd` |
| `-cmd` | `sh "cmd \|\| true"` with a note |
| `$(MAKE) -C dir [targets]`, `cd dir && $(MAKE)` | `include dir/Makefile` with root-relative path rewriting; if that import fails, delegation (§10.5) |
| `$(wildcard p)` | `glob p` |
| `$(patsubst a,b,$(x))`, `$(x:a=b)`, `$(addprefix p,$(x))`, `$(addsuffix s,$(x))`, `$(basename …)` | substitution reference |
| `filter`, `filter-out`, `notdir`, `dir`, `suffix`, `subst`, `sort`, `strip`, `word(s)`, `firstword`, `lastword`, `foreach`, `if`, `or`, `and`, `call`, `eval`, `origin`, `value` | evaluated concretely at import time |
| `$(shell …)` | with `--allow-shell`: run at import time, result inlined with a comment; otherwise an error with file:line |
| `vpath`, `VPATH` | resolved statically to concrete paths |
| `.DELETE_ON_ERROR`, `.SUFFIXES`, `.DEFAULT_GOAL`, `.SILENT`, `.NOTPARALLEL` | default goal → `default`; others ignored with a note (rmake already deletes outputs on error) |
| `::` rules, `.SECONDEXPANSION`, `$(guile …)`, `load` | v1: error with location; or delegation |

### 10.4 Output shape

The printed `Rmakefile` keeps the Makefile's order: variables first, then
rules and tasks, with original comments carried over. Make variable names are
lowercased (`CFLAGS` → `cflags`) unless that collides; CLI overrides with the
original upper-case spelling are accepted as aliases in transparent mode.

### 10.5 Delegation

For a target the importer cannot translate, it emits a rule (or task, if
phony) that runs `make -C dir target` under the rmake jobserver, marked
`always`, with a `# rmake: delegated` note. Early cutoff still applies to its
outputs.

### 10.6 Verification

- `rmake -t import --check` runs `make -n -B <goal>` and `rmake -n -B <goal>`
  on the imported build and compares the multisets of commands (whitespace
  normalized). Differences are reported per command.
- The same comparison is the importer's test harness, over a corpus in
  `testdata/makefiles/`: one minimal case per construct in §10.3, plus real
  Makefiles (small C libraries, a LaTeX document, a code generator project, a
  recursive multi-directory project). Tests that need GNU make skip if it is
  absent.

## 11. Go project layout

```
cmd/rmake/main.go
internal/syntax      lexer, parser, AST, positions, printer (= formatter)
internal/eval        vars, ?=, substitution refs, if, includes, patterns (fixpoint) → graph
internal/glob        ** globbing, builddir rule
internal/graph       nodes, edges, implicit inputs, cycle detection, topo order
internal/hash        file hashing + hash cache
internal/state       log store, records, compaction
internal/depfile     make-format depfile parser
internal/execx       process spawning, env filtering, process groups, output capture
internal/jobserver   FIFO server (Unix), fd pairs, semaphore (Windows)
internal/sched       scheduler, rebuild decision, -k, interrupts
internal/tools       why, query, graph, tasks, clean, targets, fmt, import
internal/makefile    make lexer, parser, evaluator, built-in rules, lowering → syntax AST
testdata/
```

Module `github.com/rveen/rmake`. Go 1.23 or later. No third-party
dependencies except possibly `golang.org/x/sys`.

## 12. Milestones

Each milestone ends with passing tests and a usable binary. M6–M7 depend only
on M1 and can proceed in parallel with M2–M5.

| # | Scope | Done when |
|---|---|---|
| M0 | Self-consistent spec (this document), module init | Done |
| M1 | Lexer, parser, printer, eval (`?=`, `defined`, substitution refs, rule-local vars, implicit inputs, globs, pattern fixpoint), graph, static checks; `-n`, `-t graph`, `-t targets`, `-t fmt` | §5.7 loads; errors have file:line:col; parse→print→parse round-trips; parser fuzzed |
| M2 | Serial execution, hashing, hash cache, state store, rebuild decision, `-t why`, `-t query`, `-t clean` | Second build is a no-op; flag change rebuilds; deleted/modified output rebuilds; touch-only runs nothing |
| M3 | Parallel scheduler, output capture, status line, `-k`, signal handling | Interrupt leaves no partial outputs; `-j1` and `-j16` produce identical results |
| M4 | Depfiles, `always`, early cutoff, generated includes | Header change rebuilds only affected objects; identical output skips dependents; a generated fragment is built and then used |
| M5 | Tasks with parameters, `needs`, CLI overrides and precedence, `-t tasks` | `rmake release 1.0` works; tasks never cached |
| M6 | Importer I: make lexer/parser/evaluator, variables, conditionals, includes, explicit/pattern/suffix/static-pattern rules, automatic variables, `.PHONY`, `.d` includes, GNU built-in rules, functions; `-t import`, transparent mode | Differential tests pass for the construct corpus and ≥ 3 real single-directory Makefiles |
| M7 | Importer II: recursive make → includes, `--allow-shell`, delegation, FIFO jobserver, `-t import --check` | A recursive multi-directory project imports to one graph; nested `make -j` under delegation respects the budget |
| M8 (deferred) | Windows support, fd-pair and semaphore jobserver | Not scheduled; Windows is not an objective for now |
| M9 (optional) | `--strict` access tracing (Linux), local content-addressed output cache, `tool` attribute (§14.1) | — |

## 13. Testing

- Unit tests per package. Golden-file tests for parser output, printer output,
  importer output and error messages.
- Fuzz tests: lexer/parser, printer round-trip, depfile parser, glob matcher,
  make lexer.
- Integration tests in temp directories. Use the test binary itself as a fake
  compiler (`TestMain` re-exec pattern) that copies inputs to outputs, writes
  depfiles, counts invocations. Real `cc`/`go`/`make` tests skip if the tool is absent.
- Required behavioural tests:
  - no-op second build runs zero commands;
  - `touch` without content change runs zero commands;
  - changing a variable used in a command reruns exactly the affected rules;
  - changing an implicit input reruns the rule; it does not appear in `$in`;
  - changing a discovered input (header) reruns only rules that included it;
  - a rule producing identical output does not rerun dependents;
  - externally modified output is rebuilt;
  - stale file in `$builddir` is not matched by globs;
  - failed rule leaves no outputs and no record;
  - cycle and duplicate-output errors;
  - empty-list vs `""` concatenation;
  - CLI > environment > `?=` precedence;
  - a generated include is built, then used;
  - imported Makefiles: `make -n -B` ≡ `rmake -n -B`.

## 14. Open questions

1. **Toolchain changes.** The key includes the resolved compiler path, not its
   contents. A compiler upgrade in place is not detected. Option: `tool`
   attribute that hashes the binary (cached by size+mtime).
2. **`.gitignore` in globs.** Should non-builddir globs skip git-ignored files? (v1: no.)
3. **More substitution forms.** `${x:dir}` / `${x:file}` only if import data
   shows hand-written Rmakefiles need them.
4. **Remote cache.** Out of scope; keep the state store and output hashing
   compatible with a content-addressed store later.
5. **Name.** Check `rmake` for collisions before publishing.
