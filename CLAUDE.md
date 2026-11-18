# rmake — handoff notes for the next session

rmake is a content-hash based build tool in Go with a small build language
(`Rmakefile`) that will also import Makefiles. This file is the working state
of the project: read it, then `README.md`, before changing anything.

- `README.md` is the **specification** and is authoritative. Every decision
  made during implementation is written back into it; keep doing that.
- `PLAN.md` is the original critical review and the milestone plan (✅ marks
  finished milestones).

## Status (2026-11-18)

| Milestone | State |
|---|---|
| M0 spec, module | done (`f90bc3f`) |
| M1 parser, printer, eval, graph, `-n`, `-t graph/targets/tasks/fmt` | done (`ed2cd70`) |
| M2 serial execution, hashing, state store, rebuild decision, `-t why/query/clean` | done (`fea7752`) |
| **M3** parallel scheduler, output capture, status line, `-k`, signals | **next** |
| M4 depfiles, `always`, early cutoff, generated includes | todo (early cutoff and `always` already work) |
| M5 tasks with params, `needs`, CLI overrides, `-t tasks` | mostly done already; verify against README §12 and close |
| M6–M7 Makefile importer (`internal/makefile`, `-t import`, transparent mode) | todo; depends only on M1 |
| M8 Windows, fd-pair/semaphore jobserver | **deferred by the owner** — Windows is not an objective now |
| M9 tracing, CAS cache, `tool` attribute | optional |

## Owner's decisions and working preferences

- Name `rmake`, build file `Rmakefile`; with no `Rmakefile` but a `Makefile`,
  rmake will import the Makefile (M6). Simplicity of the language without
  losing make's power is the guiding principle; no user functions or macros.
- Windows is not a goal for now: keep Windows-only code behind build tags
  (`*_unix.go` / `*_other.go` stubs) and do not implement or test it.
- Git: work on `main` directly, no feature branches; commit and push when a
  milestone is finished and the owner says so. Commit messages start with
  `Mn: ` and end with the `Co-Authored-By` trailer. The GitHub repo
  `rveen/rmake` is private.
- Go stdlib only (`golang.org/x/sys` allowed if needed), `go 1.23` in
  `go.mod` (1.24 installed).
- Before reporting a milestone done: `gofmt -l .` empty, `go vet ./...`,
  `go test ./...` and `go test -race ./...` pass, README updated, PLAN.md
  milestone marked ✅.

## Architecture

```
cmd/rmake            CLI: flags (-C -f -j -k -n -B -v -t), name=value overrides
                     interleaved with flags, tools, wiring, RMAKE_TIMING
internal/syntax      lexer (line/word based), parser, AST with positions,
                     canonical printer (Format) used by -t fmt
internal/eval        Load(Options) -> *graph.Graph: scopes, expansion,
                     includes, patterns, tasks, fixpoint over $builddir globs
internal/glob        Match (with **), Disk (walks the tree, skips hidden dirs
                     and $builddir)
internal/graph       Rule, Task, Cmd, Graph: AddRule (duplicate outputs),
                     Check (cycles, task/file collisions), Plan, WriteDOT, Files
internal/state       Store interface; binary log in .rmake/log (codec.go), flock
internal/hash        Cache: SHA-256 + stat cache + racy-clean + in-run memo +
                     Prestat (concurrent stat)
internal/execx       BaseEnv/Env filtering, Resolver (PATH cache), Run
internal/sched       Builder: decide (key + reasons), Build (serial), runRule,
                     runTask, Why, Query, Clean
```

Flow of a build (`cmd/rmake/main.go`): parse flags → start `state.Open`
in a goroutine → `eval.Load` → `Hash.Prestat(g.Files())` → `g.Plan(requests,
Hash.Exists)` → `Builder.Build(steps)` → `Store.Flush`.

## Non-obvious invariants (read before editing)

- **Evaluation is a fixpoint** (`eval.Load`): each pass evaluates the whole
  build with the outputs declared by the previous pass visible to `$builddir`
  globs. `pass.stable()` ends after one pass when every `$builddir` glob
  already matched the final output set. Max 32 passes.
- **Paths**: everything in the graph is root-relative. In included files,
  paths are rewritten relative to the file's directory, except absolute paths
  and paths under `$builddir` (always root-relative). Commands and `depfile`
  are never rewritten.
- **Expansion**: rc-style distribution; empty list kills the word, `""` does
  not; inside quotes a list joins with spaces; `${x:pat=repl}` is the only
  transform. Rule-local vars are evaluated before other attributes. Tasks
  snapshot their scope at definition.
- **Default target**: first non-pattern rule output or task in the root file.
- **Rule identity** = `sched.RuleID` (hash of sorted outputs). Bump
  `sched.KeyVersion` whenever the key composition changes.
- **Hash memo** (`hash.Cache`): within a run a memoized hash or pre-stat result
  is trusted until `Forget(path)`. Anything that rewrites a file during a build
  must call `Forget` first (runRule does, for outputs). In M3, guard the
  cache with a mutex or confine it to the scheduler goroutine.
- **State log** is valid up to the first damaged record; the rest is cut off
  on the next append. Losing state is always safe (it only causes rebuilds).
  `state.Store` is not safe for concurrent use.
- Dry run (`-n`) and `-t why` treat outputs of rules that would run as
  "will be rebuilt" (they cannot be hashed yet).

## Testing and measuring

- `go test ./...`; end-to-end behaviour tests are in `cmd/rmake/build_test.go`
  (rules append to `ran.log` so tests assert exactly which rules ran).
- Fuzz targets: `syntax.FuzzParse` (parse→format→parse idempotence),
  `glob.FuzzMatch`, `state.FuzzDecode`. Regression inputs live in
  `testdata/fuzz/`.
- Performance check (no-op goal < 100 ms): 10k files `src/fN.txt`, Rmakefile
  `rule build/%.txt from src/%.txt` + `run cp $in $out` and
  `rule build/all from build/*.txt` + `sh "cat $in > $out"`; build once, then
  `RMAKE_TIMING=1 rmake`. Last result: ~115 ms total (load 42, stat 21,
  plan 5, build 52 ms) for 10,001 rules / 20k files. `RMAKE_CPUPROFILE=f`
  writes a profile.

## Known gaps and loose ends

- `-j` is parsed but unused (M3); execution is serial. No signal handling yet
  (Ctrl-C kills rmake; state is crash-safe, but outputs of the interrupted
  rule may be partial). No process groups yet.
- Progress `[n/N]` counts all planned steps, including up-to-date ones.
- `deps`/`depfile` are parsed and stored on `graph.Rule` but depfiles are not
  read yet (M4); `state.Rule.Discovered` and the key already support them.
- Including a generated file and including Makefiles return "not implemented"
  errors (M4, M6). `-t import` likewise.
- Task `desc` is computed with placeholder arguments; for tasks with
  parameters the progress line falls back to `task NAME ARGS`. Better: have
  `Task.Expand` return the expanded desc.
- The README §5.7 example deliberately contains a duplicate-output case
  (`src/gen.c`), explained under the example; the tests use a copy without it.

## Plan for M3 (next)

1. `sched.Build`: replace the serial loop with a ready-queue scheduler over
   the plan: a step is ready when the steps producing its inputs (and needed
   tasks) are done; run up to `-j` jobs in goroutines; keep `decide`, state
   writes and hash-cache access on the scheduler goroutine (or add a mutex).
   Respect early cutoff: a dependent is decided only after its inputs settle.
2. Without `-k`: stop starting jobs after the first failure, wait for running
   ones, exit 1. With `-k`: skip only dependents of failures (already the
   semantics of the serial loop).
3. `execx`: run each command in its own process group (`Setpgid`, in a
   `_unix.go` file); on SIGINT/SIGTERM forward the signal to all groups, wait,
   delete outputs of interrupted rules, flush the state, exit 130.
4. Output: capture per job (done), print each job's block on completion;
   on a TTY one overwritten status line `[done/total] desc`, otherwise one
   line per started job; count only steps that actually run.
5. Tests: `-j1` and `-j16` produce identical outputs; interrupt leaves no
   partial outputs (send SIGINT to a child rmake process in a test).
