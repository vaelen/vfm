# vfm — Vaelen's File Manager

A two-pane terminal file manager, in the spirit of Midnight Commander and Norton
Commander, built for finding what is using your disk space.

The left pane is a directory tree. The right pane lists the current directory
in a grid (like `eza --icons --grid`, but filled top-to-bottom, then
left-to-right), with each entry's size shown next to its icon. Sizes are
calculated in the background by several workers at once, and the list re-sorts
as they arrive. Press `v` for a details view with permissions, owner, group,
and modified/created times.

## Requirements

- Go 1.25 or later to build.
- A terminal using a [Nerd Font](https://www.nerdfonts.com/) for the file icons.
- Linux (sizes come from `stat` block counts; creation times from `statx`).
- `$EDITOR` set for editing (falls back to `vi`).

## Install

```sh
go install github.com/vaelen/vfm@latest
# or, from a checkout
go build -o vfm .
```

## Usage

Run `vfm` in the directory you want to look at. There are no command-line
options.

| Key             | Action                                                     |
|-----------------|------------------------------------------------------------|
| `↑` `↓` `←` `→` | Move within the focused pane                               |
| `PgUp` `PgDn`   | Move a page at a time                                      |
| `Home` `End`    | Move to the first / last item                              |
| `Tab`           | Switch between the tree and contents panes                 |
| `Enter`         | Open the selected directory, or view the selected file     |
| `Esc`           | Clear the selection, or go up to the parent directory      |
| `Space`         | Select / unselect the item under the cursor                |
| `g`             | Go to a path (`~`, relative and absolute paths work)       |
| `f`             | Filter the contents by a glob pattern (e.g. `*.mp4`)       |
| `a`             | Show all entries (clear the filter)                        |
| `s`             | Cycle sort order: by size, modified date, or name          |
| `v`             | Switch between the grid and details views                  |
| `r`             | Refresh sizes for the current directory                    |
| `m`             | Move or rename the selected entries (like `mv`)            |
| `c`             | Copy the selected entries (like `cp -R`)                   |
| `d`             | Delete the selected entries (asks first)                   |
| `e`             | Edit the selected file in `$EDITOR`                        |
| `h`             | View the selected file as hex                              |
| `x`             | Run the selected executable (asks first)                   |
| `!`             | Run a shell command in the current directory               |
| `?`             | Show the list of commands                                  |
| `q`             | Quit                                                       |

In the tree pane, `→` expands a directory and `←` collapses it or moves to its
parent.

## Notes

- Sizes are disk usage (allocated blocks), the same as `du`, not apparent
  file length. Symlinks are not followed.
- The status bar shows the free space on the current volume, like `df`'s
  "Avail" column, and what percentage of the volume that is.
- A size shown in yellow is a lower bound: some paths inside could not be read.
- Hidden files are shown.
- Files with several hard links are counted once per link (`du` counts them once).
- Sizes are remembered for the session; press `r` to recalculate.
- Space selects several entries (shown in yellow with a `*`); `m`, `c` and `d`
  then act on all of them, otherwise on the entry under the cursor. Opening a
  directory or changing the filter clears the selection. Moving or copying
  several entries needs an existing directory as the destination.
- Delete is permanent (`rm -rf`), not a move to the trash.
- Move and copy run the system `mv` and `cp -R`. If the destination already
  exists, vfm asks before overwriting it.
- Enter on a file opens the built-in viewer: Markdown (`.md`, `.markdown`) is
  rendered, other text is shown as is, and binary files are shown as hex.
  Up to 4 MiB of a file is shown (1 MiB as hex). Press `q` or `Esc` to close.
- `x` shows the program's real location (following symlinks) and asks before
  running it. The program and the editor take over the terminal; after a
  program or `!` command exits, vfm waits for a key press before returning.
  After `!`, the current directory is re-read and re-sized.
- The details view drops the Created, Group, Owner, Permissions and Modified
  columns, in that order, when the pane is too narrow.
- Glob patterns are case-sensitive and use Go's
  [`filepath.Match`](https://pkg.go.dev/path/filepath#Match) syntax.

## License

MIT. See [LICENSE](LICENSE).
