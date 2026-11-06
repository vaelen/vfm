# vfm — Vaelen's File Manager

A two-pane terminal file manager, in the spirit of Midnight Commander and Norton
Commander, built for finding what is using your disk space.

The left pane is a directory tree. The right pane lists the current directory
in a grid (like `eza --icons --grid`, but filled top-to-bottom, then
left-to-right), with each entry's size shown next to its icon. Sizes are
calculated in the background by several workers at once, and the list re-sorts
as they arrive.

## Requirements

- Go 1.25 or later to build.
- A terminal using a [Nerd Font](https://www.nerdfonts.com/) for the file icons.
- Linux or another Unix-like system (sizes come from `stat` block counts).

## Install

```sh
go install .        # from a checkout; installs to $(go env GOPATH)/bin/vfm
# or
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
| `Enter`         | Open the selected directory                                |
| `Esc`           | Go up to the parent directory                              |
| `g`             | Go to a path (`~`, relative and absolute paths work)       |
| `f`             | Filter the contents by a glob pattern (e.g. `*.mp4`)       |
| `a`             | Show all entries (clear the filter)                        |
| `s`             | Cycle sort order: largest, smallest, name A→Z, name Z→A    |
| `r`             | Refresh sizes for the current directory                    |
| `d`             | Delete the selected file or directory (asks first)         |
| `q`             | Quit                                                       |

In the tree pane, `→` expands a directory and `←` collapses it or moves to its
parent.

## Notes

- Sizes are disk usage (allocated blocks), the same as `du`, not apparent
  file length. Symlinks are not followed.
- A size shown in yellow is a lower bound: some paths inside could not be read.
- Hidden files are shown.
- Files with several hard links are counted once per link (`du` counts them once).
- Sizes are remembered for the session; press `r` to recalculate.
- Delete is permanent (`rm -rf`), not a move to the trash.
- Glob patterns are case-sensitive and use Go's
  [`filepath.Match`](https://pkg.go.dev/path/filepath#Match) syntax.

## License

MIT. See [LICENSE](LICENSE).
