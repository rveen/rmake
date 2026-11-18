# Importer corpus

Differential test corpus for the Makefile importer (README §10.6), run by
`TestImportDifferential` in `cmd/rmk/import_test.go`: for every directory,
`make -n -B GOALS` and `rmk -n -B GOALS` (transparent mode) must print the
same multiset of commands.

- `goals`: one argument list per line (an empty line is the default goal);
  without it, the default goal only.
- `rundir`: the subdirectory make and rmake run in.

Source files are empty stubs: only their existence matters.

The `real-*` directories hold unmodified Makefiles from other projects, as
installed on a Fedora system:

| directory | origin | license |
|---|---|---|
| `real-xz` | xz `doc/examples/Makefile` | 0BSD (SPDX header) |
| `real-cmark` | goldmark v1.4.13 `_benchmark/cmark/Makefile` | MIT |
| `real-icu-coll` | ICU `samples/coll/Makefile`, `samples/defs.mk`, `samples/rules.mk` | Unicode License (headers) |
| `real-rsync` | rsync `support/Makefile` | GPL-3.0 |
| `real-vim-maze` | Vim `runtime/macros/maze/Makefile` | Vim license |

The other directories are minimal cases, one per construct group of README
§10.3.
