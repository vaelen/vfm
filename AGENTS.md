# AGENTS.md

Guidance for coding agents working on vfm (Vaelen's File Manager).

## Project

A two-pane TUI file manager written in Go with Bubbletea v1
(`github.com/charmbracelet/bubbletea`), Bubbles and Lipgloss. Its main purpose
is finding what is using disk space. See README.md for features and keys.

- `main.go`: the whole app (sizing workers, icons, directory tree, model, view).
- `main_test.go`: unit tests for the pure helpers (size formatting, sort order,
  path expansion, display sanitizing).

Keep it in as few files as practical; split only when a file becomes hard to
navigate.

## Build and test

```sh
go vet ./... && go test ./... && go build -o vfm .
```

Run `gofmt -w .` before committing.

## Testing the TUI

The app needs a real terminal. Drive it with tmux and read the screen with
`capture-pane`:

```sh
tmux new-session -d -s vfm -x 120 -y 20 -c /some/dir "$PWD/vfm"
tmux send-keys -t vfm Down; sleep 0.2
tmux capture-pane -p -t vfm
tmux send-keys -t vfm q
```

- Use an absolute path to the binary; tmux runs it from the `-c` directory.
- Pause between keys. Letters sent back-to-back arrive as one multi-rune key
  event (e.g. `"sss"`) and match no command.
- Nerd Font icons are in the capture but may not display in your output;
  check line widths if alignment looks off.
- Test destructive keys (`d`, `m`, `c`) only in a scratch directory.
- Text typed into input dialogs: `tmux send-keys -t vfm -l 'text'`; clear the
  pre-filled value first with `C-u`.
- New or changed files trigger re-sizing, which (in size sorts) moves the
  cursor back to the first item; re-select before acting on an entry.

## Conventions

- Every source file starts with this two-line header:

  ```go
  // Copyright (c) 2026 Andrew C. Young <andrew@vaelen.org>
  // SPDX-License-Identifier: MIT
  ```

- Pass every file name or path from the filesystem through `clean()` before
  rendering it, so names can't inject terminal escape sequences. Use the raw
  name for filesystem operations.
- Model state is only mutated in `Update`. Background work (sizing, deleting)
  reports back with messages; sizing results carry a generation number so
  results for a directory you've left are ignored.

## Git

- Commit messages: one or two short lines.
- Do not mention Claude Code or any AI tool, and do not add `Co-Authored-By`
  lines.
