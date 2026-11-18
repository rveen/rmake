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
| M3 parallel scheduler, output capture, status line, `-k`, signals | done (`93d074e`) |
| M4 depfiles, `always`, early cutoff, generated includes | done (`c2e7246`) |
| M5 tasks with params, `needs`, CLI overrides, `-t tasks` | done (uncommitted until the owner says so) |
| **M6**–M7 Makefile importer (`internal/makefile`, `-t import`, transparent mode) | **next** (M6); depends only on M1 |
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
internal/depfile     Parse (make-format depfiles), Normalize (root-relative)
internal/glob        Match (with **), Disk (walks the tree, skips hidden dirs
                     and $builddir)
internal/graph       Rule, Task, Cmd, Graph: AddRule (duplicate outputs),
                     Check (cycles, task/file collisions), Plan, WriteDOT, Files
internal/state       Store interface; binary log in .rmake/log (codec.go), flock
internal/hash        Cache: SHA-256 + stat cache + racy-clean + in-run memo +
                     Prestat (concurrent stat); Compute (concurrent) + Put
internal/execx       BaseEnv/Env filtering, Resolver (PATH cache, mutex), Run
                     (own process group, Interrupt cause → signal, KillAll)
internal/sched       Builder: decide (key + reasons); build.go: parallel
                     scheduler (Build, makeNodes, run.loop/decide/start/
                     complete, worker `work`); progress.go: output/status
                     line; tools.go: Why, Query, Clean
```

Flow of a build (`cmd/rmake/main.go`): parse flags → start `state.Open`
in a goroutine → `eval.Load` → if `g.Generated`: `buildIncludes` (build them,
reload once if changed; also for `-n` and tools except clean) →
`Hash.Prestat(g.Files())` → `g.Plan(requests,
Hash.Exists)` → `Builder.Build(ctx, steps)` (ctx cancelled by SIGINT/SIGTERM
with an `execx.Interrupt` cause) → `Store.Flush` → exit 0/1/130.

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
  must call `Forget` first (`run.start` does, for outputs).
- **Scheduler confinement**: `decide`, the hash cache (except `Compute`), the
  state store and all printing run on the goroutine that calls `Build`.
  Workers (`Builder.work`) only run commands into the job's buffer, mkdir and
  `Hash.Compute` outputs; `complete` then `Put`s the hashes and writes the
  record. Keep it that way rather than adding locks. Ready steps are decided
  immediately (not when a slot frees), which keeps the progress total
  accurate and up-to-date rules fast; the plan index is the priority, so
  `-j1` follows plan order and `-n` prints in plan order.
- **Depfiles**: the worker reads, parses and deletes the depfile (whatever
  the outcome); `complete` normalizes, drops declared inputs/outputs, hashes
  the rest with `Hash.Hash` (missing → `missingHash`) and recomputes the key
  with the new discovered set before `PutRule`. `decide` uses the previous
  record's discovered set, so the two keys match on a no-op.
- **Tasks**: `Task.Expand(args)` returns commands, pass'ed env and the desc
  expanded with the arguments; `Task.Desc` keeps the `<param>` placeholder
  form for `-t tasks`. Tasks are ordered only by `needs` (parallel under -j).
- **Generated includes**: `eval` reads a missing include as empty and
  `pass.finish` resolves `g.Generated` (includes that are rule outputs) or
  errors for a missing include nobody produces. `Builder.Build` no longer
  prints "no work to do"; it sets `Builder.Worked` and main prints it, since
  a run may call Build twice.
- **State log** is valid up to the first damaged record; the rest is cut off
  on the next append. Losing state is always safe (it only causes rebuilds).
  `state.Store` is not safe for concurrent use.
- Dry run (`-n`) and `-t why` treat outputs of rules that would run as
  "will be rebuilt" (they cannot be hashed yet).

## Testing and measuring

- `go test ./...`; end-to-end behaviour tests are in `cmd/rmake/build_test.go`,
  `cmd/rmake/m5_test.go` (tasks, precedence through the real environment),
  `cmd/rmake/m4_test.go` (depfiles with a fake `cc.sh` and with real `cc`
  when present, `always`, generated includes)
  (rules append to `ran.log` so tests assert exactly which rules ran), and
  in `cmd/rmake/parallel_test.go` (`-j1` ≡ `-j16`, real parallelism, failure
  without `-k`, SIGINT to a child rmake: `TestMain` makes the test binary act
  as rmake when `RMAKE_TEST_MAIN` is set).
- Fuzz targets: `syntax.FuzzParse` (parse→format→parse idempotence),
  `glob.FuzzMatch`, `state.FuzzDecode`, `depfile.FuzzParse`. Regression inputs live in
  `testdata/fuzz/`.
- Performance check (no-op goal < 100 ms): 10k files `src/fN.txt`, Rmakefile
  `rule build/%.txt from src/%.txt` + `run cp $in $out` and
  `rule build/all from build/*.txt` + `sh "cat $in > $out"`; build once, then
  `RMAKE_TIMING=1 rmake`. Last result (M3): ~101 ms total (load 34, stat
  16, plan 3, build 50 ms) for 10,001 rules / 20k files; the full build of
  the same project takes 1.8 s with `-j6` vs 23 s serially. Measure no-op
  builds more than 2 s after the last build, or racy-clean rehashing
  inflates them. `RMAKE_CPUPROFILE=f` writes a profile.

## Known gaps and loose ends

- Signals are only handled during `Build`; a Ctrl-C while loading or
  planning kills rmake the default way (nothing runs then, state is safe).
- Rebuild decisions hash inputs on the scheduler goroutine; a rule with huge
  changed inputs delays starting other jobs (could move to workers later).
- `Builder.Ran` (commands started) is only for tests/statistics.
- The no-op build of the 10k benchmark is ~3 ms slower than the serial M2
  loop (dependency edges + heaps), ~101 ms total: right at the 100 ms goal.
  Load (34 ms) and decide/key hashing are the places to look.
- `deps`/`depfile` are parsed and stored on `graph.Rule` but depfiles are not
  read yet (M4); `state.Rule.Discovered` and the key already support them.
- Including Makefiles returns a "not implemented" error (M6); `-t import`
  likewise.
- With `-n`, a run whose only work was rebuilding a generated include does
  not print "no work to do" even if the dry run itself lists nothing.
- Variables defined in a generated fragment are local to it (README §5.5),
  so fragments contribute rules and tasks, not settings for the parent.
- The README §5.7 example deliberately contains a duplicate-output case
  (`src/gen.c`), explained under the example; the tests use a copy without it.

## Plan for M6 (next)

Makefile importer I (README §10, PLAN.md §5–6): new package
`internal/makefile` (make lexer/parser/evaluator: variables, conditionals,
includes, explicit/pattern/suffix/static-pattern rules, automatic variables,
`.PHONY` → task, `-include *.d` → `deps gcc`, GNU built-in rules, common
functions), emitting `syntax` AST so `syntax.Format` prints it. Wire up
`-t import`, `include x.mk`/`Makefile` (eval.include currently errors) and
transparent mode (`buildFile` in main: no Rmakefile but a Makefile).
Acceptance: differential tests (`make -n -B` ≡ `rmake -n -B`) for a
construct corpus and ≥ 3 real single-directory Makefiles; skip if `make` is
absent. Large milestone: agree a scope cut with the owner first.
