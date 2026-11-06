// Copyright (c) 2026 Andrew C. Young <andrew@vaelen.org>
// SPDX-License-Identifier: MIT

// vfm (Vaelen's File Manager): a two-pane TUI file manager that sizes everything in the current directory.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
)

// ---------- sizes ----------

const units = "BKMGTP"

// fmtSize renders n as a fixed 9-column "123.45 M" string so sizes line up.
func fmtSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%7d B", n)
	}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%7.2f %c", f, units[i])
}

// diskUsage sums allocated blocks like du (not apparent size). Symlinks are not followed.
// partial reports that some paths could not be read.
// ponytail: hardlinks are counted once per link, unlike du; dedupe on (dev, ino) if it matters.
func diskUsage(ctx context.Context, path string) (size int64, partial bool) {
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			partial = true
			return nil
		}
		info, err := d.Info()
		if err != nil {
			partial = true
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			size += st.Blocks * 512
		}
		return nil
	})
	return size, partial
}

var program *tea.Program // set in main; sizing workers report through it

type sizingMsg struct {
	gen  int
	name string
}

type sizedMsg struct {
	gen     int
	name    string
	size    int64
	partial bool
}

// sizeAll sizes each named entry of dir on a pool of workers, reporting start and finish of each.
func sizeAll(ctx context.Context, gen int, dir string, names []string) {
	jobs := make(chan string)
	go func() {
		defer close(jobs)
		for _, n := range names {
			select {
			case jobs <- n:
			case <-ctx.Done():
				return
			}
		}
	}()
	for range runtime.NumCPU() {
		go func() {
			for name := range jobs {
				program.Send(sizingMsg{gen, name})
				size, partial := diskUsage(ctx, filepath.Join(dir, name))
				if ctx.Err() != nil {
					return
				}
				program.Send(sizedMsg{gen, name, size, partial})
			}
		}()
	}
}

// ---------- icons ----------

var extIcons = map[string]rune{
	".go": '', ".py": '', ".js": '', ".ts": '', ".json": '',
	".md": '', ".txt": '', ".log": '', ".pdf": '',
	".sh": '', ".bash": '', ".zsh": '', ".fish": '',
	".html": '', ".css": '', ".rs": '', ".c": '', ".cpp": '', ".h": '',
	".java": '', ".rb": '', ".lua": '',
	".yml": '', ".yaml": '', ".toml": '', ".ini": '', ".conf": '', ".lock": '',
	".zip": '', ".tar": '', ".gz": '', ".tgz": '', ".xz": '', ".bz2": '',
	".zst": '', ".7z": '', ".rar": '', ".deb": '', ".rpm": '', ".jar": '',
	".iso": '', ".img": '', ".qcow2": '', ".vdi": '', ".vmdk": '',
	".png": '', ".jpg": '', ".jpeg": '', ".gif": '', ".svg": '', ".webp": '',
	".mp4": '', ".mkv": '', ".mov": '', ".avi": '', ".webm": '',
	".mp3": '', ".flac": '', ".wav": '', ".ogg": '', ".m4a": '',
}

func icon(name string, dir, link bool) string {
	switch {
	case dir && name == ".git":
		return ""
	case dir:
		return ""
	case link:
		return ""
	case name == "Dockerfile":
		return ""
	}
	if r, ok := extIcons[strings.ToLower(filepath.Ext(name))]; ok {
		return string(r)
	}
	return ""
}

// ---------- directory tree ----------

type node struct {
	name, path       string
	parent           *node
	kids             []*node
	loaded, expanded bool
}

func (n *node) load() {
	if n.loaded {
		return
	}
	n.loaded = true
	ents, _ := os.ReadDir(n.path)
	for _, e := range ents {
		if e.IsDir() {
			n.kids = append(n.kids, &node{name: e.Name(), path: filepath.Join(n.path, e.Name()), parent: n})
		}
	}
}

type treeRow struct {
	n     *node
	depth int
}

// ---------- model ----------

const (
	queued = iota
	sizing
	sized
)

type item struct {
	name      string
	dir, link bool
	state     int
	size      int64
	partial   bool
	meta      *meta // loaded lazily by metaOf for the details view
}

// ---------- details view metadata ----------

type meta struct {
	mode         fs.FileMode
	owner, group string
	mtime, btime time.Time
}

var idNames = map[string]string{} // "u1000"/"g1000" → name; only touched on the UI goroutine

func idName(kind byte, id uint32) string {
	key := string(kind) + strconv.FormatUint(uint64(id), 10)
	if n, ok := idNames[key]; ok {
		return n
	}
	n := key[1:]
	if kind == 'u' {
		if u, err := user.LookupId(n); err == nil {
			n = u.Username
		}
	} else if g, err := user.LookupGroupId(n); err == nil {
		n = g.Name
	}
	idNames[key] = n
	return n
}

func loadMeta(path string) *meta {
	x := &meta{}
	fi, err := os.Lstat(path)
	if err != nil {
		return x
	}
	x.mode, x.mtime = fi.Mode(), fi.ModTime()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		x.owner, x.group = idName('u', st.Uid), idName('g', st.Gid)
	}
	// Creation (birth) time needs statx; filesystems that don't record it leave it zero.
	var sx unix.Statx_t
	if unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &sx) == nil && sx.Mask&unix.STATX_BTIME != 0 {
		x.btime = time.Unix(sx.Btime.Sec, int64(sx.Btime.Nsec))
	}
	return x
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

type detailCol struct {
	title string
	w     int
	val   func(*meta) string
}

// Columns between Size and Name, in display order.
var detailCols = []detailCol{
	{"Permissions", 11, func(x *meta) string { return x.mode.String() }},
	{"Owner", 8, func(x *meta) string { return x.owner }},
	{"Group", 8, func(x *meta) string { return x.group }},
	{"Modified", 16, func(x *meta) string { return fmtTime(x.mtime) }},
	{"Created", 16, func(x *meta) string { return fmtTime(x.btime) }},
}

// Columns dropped first when the pane is too narrow.
var detailDropOrder = []string{"Created", "Group", "Owner", "Permissions", "Modified"}

const (
	byLargest = iota
	bySmallest
	byNameAsc
	byNameDesc
)

var sortNames = []string{"largest", "smallest", "name A→Z", "name Z→A"}

// less orders items for a sort mode. Size sorts put unsized items at the bottom by name.
func less(a, b *item, mode int) bool {
	switch mode {
	case byNameAsc:
		return a.name < b.name
	case byNameDesc:
		return a.name > b.name
	}
	if (a.state == sized) != (b.state == sized) {
		return a.state == sized
	}
	if a.state == sized && a.size != b.size {
		return (a.size > b.size) == (mode == byLargest)
	}
	return a.name < b.name
}

// expandPath resolves ~ and paths relative to cwd.
func expandPath(p, cwd string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

type sizeResult struct {
	size    int64
	partial bool
}

type openMsg string

type model struct {
	w, h  int
	focus int // 0 = tree, 1 = contents
	start string

	root       *node
	tcur, toff int // tree cursor and scroll offset (rows)

	dir        string
	all        []*item // every entry in dir
	items      []*item // entries shown: all, filtered and sorted
	byName     map[string]*item
	filter     string // glob pattern; empty shows everything
	sortMode   int
	ccur, coff int // contents cursor and scroll offset (columns)
	dirty      bool
	pinned     bool // cursor follows its item through re-sorts (set once the user picks one)
	gen        int
	cancel     context.CancelFunc
	cache      map[string]sizeResult // full path → size; cleared by refresh/delete via forget

	view int // gridView or detailsView
	roff int // details view scroll offset (rows)
	help bool

	viewer *viewer // full-screen file viewer, when open

	confirm *confirmation // yes/no dialog awaiting an answer
	busy    string        // long-running operation shown in the status bar

	prompt, promptTitle, promptSrc string // open input dialog kind, its title, and the path it acts on
	input                          textinput.Model

	spin spinner.Model
	err  string
}

const (
	gridView = iota
	detailsView
)

// confirmation is a yes/no dialog; run is started (with busy shown) only on "y".
type confirmation struct {
	title, path, desc, yes, busy string
	run                          tea.Cmd
}

type deletedMsg struct {
	path string
	err  error
}

// opDoneMsg reports a finished mv/cp of src to dst (the final path).
type opDoneMsg struct {
	src, dst string
	move     bool
	out      string
	err      error
}

// execDoneMsg reports return from a suspended external program; forget is a path whose size may have changed.
type execDoneMsg struct {
	err    error
	forget string
}

// under reports whether path is dir or inside it.
func under(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// reload re-reads a tree node's subdirectories, keeping existing child nodes (and their expansion).
func (n *node) reload() {
	old := map[string]*node{}
	for _, k := range n.kids {
		old[k.name] = k
	}
	n.loaded, n.kids = false, nil
	n.load()
	for i, k := range n.kids {
		if o, ok := old[k.name]; ok {
			n.kids[i] = o
		}
	}
}

func (m *model) reloadNode(path string) {
	if n := m.walkTo(path, false); n.path == path {
		n.reload()
	}
}

func (m *model) metaOf(it *item) *meta {
	if it.meta == nil {
		it.meta = loadMeta(filepath.Join(m.dir, it.name))
	}
	return it.meta
}

// pausedCmd runs args in dir, then waits for Enter so the output stays visible.
func pausedCmd(dir string, args ...string) *exec.Cmd {
	script := `"$@"; s=$?; printf '\n[exit %d] Press Enter to return to vfm ' "$s"; read _`
	c := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	c.Dir = dir
	return c
}

// run suspends the TUI and runs c in the terminal.
func (m *model) run(c *exec.Cmd, forget string) tea.Cmd {
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{err, forget} })
}

// activate handles Enter on a path: open directories (including symlinked ones), view files.
func (m *model) activate(path string) tea.Cmd {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		m.err = err.Error()
	case fi.IsDir():
		return m.open(path, "")
	case fi.Mode().IsRegular():
		m.openViewer(path, false)
	default:
		m.err = "not a regular file: " + path
	}
	return nil
}

// confirmExec asks before running path; for a symlink the dialog shows (and runs) the real target.
func (m *model) confirmExec(path string) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		m.err = err.Error()
		return
	}
	if fi, err := os.Stat(real); err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		m.err = "not an executable file: " + path
		return
	}
	desc := "runs in " + m.dir
	if real != path {
		desc = "via symlink " + filepath.Base(path)
	}
	m.confirm = &confirmation{title: "Run this program?", path: real, desc: desc, yes: "Run",
		run: m.run(pausedCmd(m.dir, real), "")}
}

// ---------- file viewer ----------

const (
	viewLimit = 4 << 20 // bytes of a file read into the viewer
	hexLimit  = 1 << 20 // bytes shown in the hex viewer
)

var mdStyle = "dark" // glamour style; set from the terminal background before the TUI starts

type viewer struct {
	title string
	vp    viewport.Model
}

// isBinary uses git's heuristic on the first 8 KB: a NUL byte or invalid UTF-8 means binary.
func isBinary(data []byte) bool {
	sample := data[:min(len(data), 8000)]
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if len(data) > len(sample) { // don't count a rune cut off by the sample boundary
		for i := 0; i < 3 && !utf8.Valid(sample); i++ {
			sample = sample[:len(sample)-1]
		}
	}
	return !utf8.Valid(sample)
}

func expandTabs(s string) string {
	var b strings.Builder
	col := 0
	for _, r := range s {
		switch r {
		case '\t':
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case '\n':
			b.WriteRune(r)
			col = 0
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

func renderMarkdown(src string, width int) string {
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(mdStyle), glamour.WithWordWrap(width))
	if err != nil {
		return expandTabs(src)
	}
	out, err := r.Render(src)
	if err != nil {
		return expandTabs(src)
	}
	return out
}

// openViewer shows a file full screen: markdown rendered, other text as is, binary (or forceHex) as hex.
// ponytail: reads up to viewLimit into memory; stream from disk if huge files need viewing.
func (m *model) openViewer(path string, forceHex bool) {
	f, err := os.Open(path)
	if err != nil {
		m.err = err.Error()
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, viewLimit+1))
	if err != nil {
		m.err = err.Error()
		return
	}
	truncated := len(data) > viewLimit
	data = data[:min(len(data), viewLimit)]
	kind, content := "text", ""
	switch ext := strings.ToLower(filepath.Ext(path)); {
	case forceHex || isBinary(data):
		kind = "hex"
		if len(data) > hexLimit {
			data, truncated = data[:hexLimit], true
		}
		content = hex.Dump(data)
	case ext == ".md" || ext == ".markdown":
		kind, content = "markdown", renderMarkdown(cleanText(string(data)), m.w-4)
	default:
		content = expandTabs(cleanText(string(data)))
	}
	title := clean(path) + "   [" + kind + "]"
	if truncated {
		title += fmt.Sprintf("   (first %d MiB)", len(data)>>20)
	}
	vp := viewport.New(m.w-2, m.innerH())
	vp.SetContent(content)
	m.viewer = &viewer{title, vp}
}

func (m *model) viewerKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "esc":
		m.viewer = nil
	case "home":
		m.viewer.vp.GotoTop()
	case "end":
		m.viewer.vp.GotoBottom()
	default:
		var cmd tea.Cmd
		m.viewer.vp, cmd = m.viewer.vp.Update(msg)
		return cmd
	}
	return nil
}

// transfer runs mv or cp -R from src to the user's destination, confirming before overwriting.
func (m *model) transfer(kind, src, v string) tea.Cmd {
	if v == "" {
		return nil
	}
	dst := expandPath(v, m.dir)
	target := dst // the path src will end up at
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		target = filepath.Join(dst, filepath.Base(src))
	}
	if target == src {
		m.err = "source and destination are the same"
		return nil
	}
	name, args, verb := "mv", []string{"--", src, dst}, "Moving"
	if kind == "copy" {
		name, args, verb = "cp", []string{"-R", "--", src, dst}, "Copying"
	}
	run := func() tea.Msg {
		out, err := exec.Command(name, args...).CombinedOutput()
		return opDoneMsg{src, target, kind == "move", string(out), err}
	}
	busy := verb + " " + src + "…"
	if _, err := os.Lstat(target); err == nil {
		m.confirm = &confirmation{title: "Overwrite?", path: target, desc: "already exists", yes: "Overwrite", busy: busy, run: run}
		return nil
	}
	m.busy = busy
	return run
}

// forget drops cached sizes for path, everything under it, and its ancestors (whose totals include it).
func (m *model) forget(path string) {
	prefix := strings.TrimSuffix(path, "/") + "/"
	for k := range m.cache {
		if k == path || strings.HasPrefix(k, prefix) || strings.HasPrefix(path, k+"/") {
			delete(m.cache, k)
		}
	}
}

// selected returns the path under the cursor in the focused pane.
func (m *model) selected() string {
	if m.focus == 0 {
		return m.treeRows()[m.tcur].n.path
	}
	if m.ccur < len(m.items) {
		return filepath.Join(m.dir, m.items[m.ccur].name)
	}
	return ""
}

// reopen re-reads the current directory, keeping the cursor at the same position.
func (m *model) reopen(dir string) tea.Cmd {
	idx := m.ccur
	cmd := m.open(dir, "")
	if dir == m.dir {
		m.ccur, m.pinned = idx, true
	}
	return cmd
}

const (
	sizeW = 9 // width of fmtSize output
	gap   = 2 // space between grid columns
)

func (m *model) treeW() int    { return min(40, m.w/3) }
func (m *model) innerH() int   { return max(1, m.h-3) } // minus status bar and two borders
func (m *model) contentW() int { return max(1, m.w-m.treeW()-2) }
func cellWidth(it *item) int   { return sizeW + 3 + ansi.StringWidth(clean(it.name)) }

// sortItems rebuilds the visible list from all entries: filter, then sort.
func (m *model) sortItems() {
	items := make([]*item, 0, len(m.all))
	for _, it := range m.all {
		if ok, _ := filepath.Match(m.filter, it.name); ok || m.filter == "" {
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j], m.sortMode) })
	m.items = items
}
func (m *model) treeIndex(n *node) int {
	for i, r := range m.treeRows() {
		if r.n == n {
			return i
		}
	}
	return 0
}

func (m *model) treeRows() []treeRow {
	var rows []treeRow
	var walk func(*node, int)
	walk = func(n *node, d int) {
		rows = append(rows, treeRow{n, d})
		if n.expanded {
			for _, k := range n.kids {
				walk(k, d+1)
			}
		}
	}
	walk(m.root, 0)
	return rows
}

// reveal expands the tree down to dir and puts the tree cursor on it.
func (m *model) reveal(dir string) { m.tcur = m.treeIndex(m.walkTo(dir, true)) }

// walkTo returns the tree node for path (or its deepest existing ancestor), loading nodes on the way.
func (m *model) walkTo(path string, expand bool) *node {
	n := m.root
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			break
		}
		n.load()
		n.expanded = n.expanded || expand
		var next *node
		for _, k := range n.kids {
			if k.name == part {
				next = k
			}
		}
		if next == nil {
			break
		}
		n = next
	}
	return n
}

// open switches the contents pane to dir, selecting sel if present, and starts sizing.
func (m *model) open(dir, sel string) tea.Cmd {
	ents, err := os.ReadDir(dir)
	if err != nil {
		m.err = err.Error()
		return nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.gen++
	m.cancel, m.dir, m.dirty, m.pinned = cancel, dir, false, false
	m.all, m.byName = make([]*item, 0, len(ents)), make(map[string]*item, len(ents))
	var todo []string
	for _, e := range ents {
		it := &item{name: e.Name(), dir: e.IsDir(), link: e.Type()&fs.ModeSymlink != 0}
		if r, ok := m.cache[filepath.Join(dir, it.name)]; ok {
			it.state, it.size, it.partial = sized, r.size, r.partial
		} else {
			todo = append(todo, it.name)
		}
		m.all = append(m.all, it)
		m.byName[it.name] = it
	}
	m.sortItems()
	m.ccur, m.coff = 0, 0
	for i, it := range m.items {
		if it.name == sel {
			m.ccur, m.pinned = i, true
		}
	}
	m.reveal(dir)
	gen := m.gen
	return func() tea.Msg { sizeAll(ctx, gen, dir, todo); return nil }
}

// resort re-filters and re-sorts the contents, keeping the cursor on the same item
// (or the top, if the user hasn't picked one or it was filtered out).
func (m *model) resort() {
	var sel *item
	if m.pinned && m.ccur < len(m.items) {
		sel = m.items[m.ccur]
	}
	m.sortItems()
	m.ccur = 0
	for i, it := range m.items {
		if it == sel {
			m.ccur = i
		}
	}
	m.dirty = false
}

// ask opens an input dialog of the given kind.
func (m *model) ask(kind, title, value string) tea.Cmd {
	m.prompt, m.promptTitle, m.promptSrc = kind, title, m.selected()
	m.input.Width = max(10, min(60, m.w-16))
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m.input.Focus()
}

func (m *model) promptKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.prompt = ""
		return nil
	case "enter":
		kind, v := m.prompt, strings.TrimSpace(m.input.Value())
		m.prompt = ""
		return m.submit(kind, v)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *model) submit(kind, v string) tea.Cmd {
	switch kind {
	case "filter":
		if _, err := filepath.Match(v, ""); err != nil {
			m.err = "bad pattern: " + v
			return nil
		}
		m.filter = v
		m.resort()
	case "goto":
		if v == "" {
			return nil
		}
		p := expandPath(v, m.dir)
		fi, err := os.Stat(p)
		if err != nil {
			m.err = err.Error()
			return nil
		}
		if !fi.IsDir() {
			return m.open(filepath.Dir(p), filepath.Base(p)) // a file: open its directory with it selected
		}
		return m.open(p, "")
	case "move", "copy":
		return m.transfer(kind, m.promptSrc, v)
	}
	return nil
}

func (m *model) lookup(gen int, name string) *item {
	if gen != m.gen {
		return nil
	}
	return m.byName[name]
}

func (m model) Init() tea.Cmd {
	start := m.start
	return tea.Batch(m.spin.Tick, func() tea.Msg { return openMsg(start) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.clamp()
	return m, cmd
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.viewer != nil {
			m.viewer.vp.Width, m.viewer.vp.Height = m.w-2, m.innerH()
		}
	case openMsg:
		return m.open(string(msg), "")
	case sizingMsg:
		if it := m.lookup(msg.gen, msg.name); it != nil {
			it.state = sizing
		}
	case sizedMsg:
		if it := m.lookup(msg.gen, msg.name); it != nil {
			it.state, it.size, it.partial = sized, msg.size, msg.partial
			m.cache[filepath.Join(m.dir, it.name)] = sizeResult{msg.size, msg.partial}
			m.dirty = true
		}
	case spinner.TickMsg:
		// Re-sort at most once per spinner frame rather than once per result.
		// While sizes are arriving (including the final one), a size sort re-sorts and keeps the
		// first item selected. Name sorts don't depend on size, so nothing moves.
		if m.dirty {
			m.dirty = false
			if m.sortMode == byLargest || m.sortMode == bySmallest {
				m.resort()
				m.ccur, m.pinned = 0, false
			}
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd
	case deletedMsg:
		m.busy = ""
		if msg.err != nil {
			m.err = msg.err.Error()
		} else if n := m.walkTo(msg.path, false); n.path == msg.path && n.parent != nil {
			n.parent.kids = slices.DeleteFunc(n.parent.kids, func(k *node) bool { return k == n })
		}
		m.forget(msg.path)
		dir := m.dir
		if under(dir, msg.path) {
			dir = filepath.Dir(msg.path)
		}
		return m.reopen(dir)
	case opDoneMsg:
		m.busy = ""
		if msg.err != nil {
			if m.err = strings.TrimSpace(msg.out); m.err == "" {
				m.err = msg.err.Error()
			}
		}
		dir := m.dir
		if msg.move {
			m.forget(msg.src)
			m.reloadNode(filepath.Dir(msg.src))
			if under(dir, msg.src) {
				dir = filepath.Dir(msg.src)
			}
		}
		m.forget(msg.dst)
		m.reloadNode(filepath.Dir(msg.dst))
		return m.reopen(dir)
	case execDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		if msg.forget != "" {
			m.forget(msg.forget)
		}
		return m.reopen(m.dir)
	case tea.KeyMsg:
		m.err = ""
		if m.prompt != "" && msg.String() != "ctrl+c" {
			return m.promptKey(msg)
		}
		if m.viewer != nil && msg.String() != "ctrl+c" {
			return m.viewerKey(msg)
		}
		return m.key(msg.String())
	}
	return nil
}

func (m *model) key(k string) tea.Cmd {
	if k == "ctrl+c" {
		return tea.Quit
	}
	if m.help {
		m.help = false // any key closes help
		return nil
	}
	if c := m.confirm; c != nil {
		m.confirm = nil
		if k != "y" && k != "Y" {
			return nil // anything but y cancels
		}
		m.busy = c.busy
		return c.run
	}
	switch k {
	case "q":
		return tea.Quit
	case "?":
		m.help = true
		return nil
	case "v":
		m.view ^= 1
		return nil
	case "r":
		m.forget(m.dir)
		m.reloadNode(m.dir)
		return m.reopen(m.dir)
	case "d":
		path := m.selected()
		if path == "" || path == "/" || m.busy != "" {
			return nil
		}
		fi, err := os.Lstat(path)
		if err != nil {
			m.err = err.Error()
			return nil
		}
		desc := "file"
		if fi.IsDir() {
			desc = "directory and everything in it"
		}
		if r, ok := m.cache[path]; ok {
			desc += ", " + strings.TrimSpace(fmtSize(r.size))
		}
		m.confirm = &confirmation{title: "Delete permanently?", path: path, desc: desc, yes: "Delete",
			busy: "Deleting " + path + "…", run: func() tea.Msg { return deletedMsg{path, os.RemoveAll(path)} }}
		return nil
	case "m", "c":
		path := m.selected()
		if path == "" || path == "/" || m.busy != "" {
			return nil
		}
		name := filepath.Base(path)
		if clean(name) != name {
			name = "" // don't echo control characters into the input
		}
		if k == "m" {
			return m.ask("move", "Move / rename to", name)
		}
		return m.ask("copy", "Copy to", name)
	case "e":
		path := m.selected()
		if path == "" {
			return nil
		}
		ed := strings.Fields(os.Getenv("EDITOR"))
		if len(ed) == 0 {
			ed = []string{"vi"}
		}
		c := exec.Command(ed[0], append(ed[1:], path)...)
		c.Dir = m.dir
		return m.run(c, path)
	case "h":
		path := m.selected()
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			m.err = "not a regular file: " + path
			return nil
		}
		m.openViewer(path, true)
		return nil
	case "x":
		if path := m.selected(); path != "" {
			m.confirmExec(path)
		}
		return nil
	case "f":
		return m.ask("filter", "Filter by glob pattern", m.filter)
	case "a":
		m.filter = ""
		m.resort()
		return nil
	case "g":
		return m.ask("goto", "Go to path", "")
	case "s":
		m.sortMode = (m.sortMode + 1) % len(sortNames)
		m.resort()
		return nil
	case "tab":
		m.focus ^= 1
		return nil
	case "esc":
		if parent := filepath.Dir(m.dir); parent != m.dir {
			return m.open(parent, filepath.Base(m.dir))
		}
		return nil
	}
	if m.focus == 0 {
		return m.treeKey(k)
	}
	return m.contentKey(k)
}

func (m *model) treeKey(k string) tea.Cmd {
	rows := m.treeRows()
	n := rows[m.tcur].n
	switch k {
	case "up":
		m.tcur = max(0, m.tcur-1)
	case "down":
		m.tcur = min(len(rows)-1, m.tcur+1)
	case "pgup":
		m.tcur = max(0, m.tcur-m.innerH())
	case "pgdown":
		m.tcur = min(len(rows)-1, m.tcur+m.innerH())
	case "home":
		m.tcur = 0
	case "end":
		m.tcur = len(rows) - 1
	case "right":
		n.load()
		if !n.expanded {
			n.expanded = len(n.kids) > 0
		} else if len(n.kids) > 0 {
			m.tcur++
		}
	case "left":
		if n.expanded {
			n.expanded = false
		} else if n.parent != nil {
			m.tcur = m.treeIndex(n.parent)
		}
	case "enter":
		return m.open(n.path, "")
	}
	return nil
}

// Contents are laid out column-major: index i is at column i/rows, row i%rows.
func (m *model) contentKey(k string) tea.Cmd {
	n, rows := len(m.items), m.innerH()
	m.pinned = true
	switch k {
	case "up":
		m.ccur = max(0, m.ccur-1)
	case "down":
		m.ccur = min(n-1, m.ccur+1)
	case "pgup", "pgdown":
		page := m.innerH() - 1 // details: one screen of rows below the header
		if m.view == gridView {
			page = m.visibleCols(m.colWidths()) * rows
		}
		if k == "pgup" {
			m.ccur = max(0, m.ccur-page)
		} else {
			m.ccur = min(n-1, m.ccur+page)
		}
	case "home":
		m.ccur = 0
	case "end":
		m.ccur = n - 1
	case "left":
		if m.view == gridView && m.ccur >= rows {
			m.ccur -= rows
		}
	case "right":
		if m.view == gridView && m.ccur/rows < (n-1)/rows {
			m.ccur = min(n-1, m.ccur+rows)
		}
	case "enter":
		if m.ccur < n {
			return m.activate(filepath.Join(m.dir, m.items[m.ccur].name))
		}
	}
	return nil
}

func (m *model) colWidths() []int {
	h, iw := m.innerH(), m.contentW()
	ws := make([]int, (len(m.items)+h-1)/h)
	for i, it := range m.items {
		ws[i/h] = max(ws[i/h], min(cellWidth(it), iw))
	}
	return ws
}

// clamp keeps cursors in range and scrolls each pane so its cursor is visible.
func (m *model) clamp() {
	if m.w == 0 {
		return
	}
	h := m.innerH()
	nrows := len(m.treeRows())
	m.tcur = max(0, min(m.tcur, nrows-1))
	m.toff = max(0, min(m.toff, nrows-h))
	if m.tcur < m.toff {
		m.toff = m.tcur
	}
	if m.tcur >= m.toff+h {
		m.toff = m.tcur - h + 1
	}

	m.ccur = max(0, min(m.ccur, len(m.items)-1))
	if m.view == detailsView {
		page := max(1, h-1)
		m.roff = max(0, min(m.roff, len(m.items)-page))
		if m.ccur < m.roff {
			m.roff = m.ccur
		}
		if m.ccur >= m.roff+page {
			m.roff = m.ccur - page + 1
		}
		return
	}
	ws := m.colWidths()
	col := m.ccur / h
	m.coff = max(0, min(m.coff, len(ws)-1))
	if col < m.coff {
		m.coff = col
	}
	for m.coff < col {
		used := gap * (col - m.coff)
		for _, w := range ws[m.coff : col+1] {
			used += w
		}
		if used <= m.contentW() {
			break
		}
		m.coff++
	}
}

// ---------- view ----------

var (
	dimStyle    = lipgloss.NewStyle().Faint(true)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	dirStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	linkStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	plainStyle  = lipgloss.NewStyle()
	curStyle    = lipgloss.NewStyle().Reverse(true)
	curDimStyle = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	barStyle    = lipgloss.NewStyle().Reverse(true)
	errBarStyle = lipgloss.NewStyle().Background(lipgloss.Color("1")).Foreground(lipgloss.Color("15"))
	headerStyle = lipgloss.NewStyle().Bold(true).Underline(true)
)

// clean replaces control characters in s with '?' so names from the filesystem can't inject
// terminal escape sequences. Invalid UTF-8 (e.g. a raw 8-bit CSI byte) becomes U+FFFD.
// Display only: filesystem operations keep using the raw name.
func clean(s string) string { return strings.Map(sanitize, s) }

// cleanText is clean for file contents: newlines and tabs are kept, CRLF becomes LF.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		return sanitize(r)
	}, strings.ReplaceAll(s, "\r\n", "\n"))
}

func sanitize(r rune) rune {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return '?'
	}
	return r
}

// fit truncates or pads plain text to exactly w cells.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func truncLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && ansi.StringWidth(string(r))+1 > w {
		r = r[1:]
	}
	return "…" + string(r)
}

func (m *model) cursorStyle(pane int) lipgloss.Style {
	if m.focus == pane {
		return curStyle
	}
	return curDimStyle
}

// box draws a rounded border of outer size w×h with title set into the top edge.
func box(title, body string, w, h int, focused bool) string {
	c := lipgloss.Color("8")
	if focused {
		c = lipgloss.Color("12")
	}
	bc := lipgloss.NewStyle().Foreground(c)
	title = truncLeft(title, max(1, w-6))
	top := bc.Render("╭─ ") + lipgloss.NewStyle().Bold(true).Render(title) +
		bc.Render(" "+strings.Repeat("─", max(0, w-5-ansi.StringWidth(title)))+"╮")
	rest := lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), false, true, true, true).
		BorderForeground(c).Width(w - 2).Height(h - 2).Render(body)
	return top + "\n" + rest
}

func (m *model) treeLine(r treeRow, cur bool, w int) string {
	n := r.n
	mark := "▸ "
	if n.expanded {
		mark = "▾ "
	} else if n.loaded && len(n.kids) == 0 {
		mark = "  "
	}
	text := fit(strings.Repeat("  ", r.depth)+mark+icon(n.name, true, false)+" "+clean(n.name), w)
	switch {
	case cur:
		return m.cursorStyle(0).Render(text)
	case n.path == m.dir:
		return dirStyle.Render(text)
	}
	return text
}

// sizeField is the fixed-width size text for it (a spinner while sizing) and its style.
func (m *model) sizeField(it *item) (string, lipgloss.Style) {
	switch it.state {
	case sizing:
		return strings.Repeat(" ", sizeW-1) + m.spin.View(), dimStyle
	case sized:
		if it.partial {
			return fmtSize(it.size), warnStyle // some paths were unreadable; size is a lower bound
		}
		return fmtSize(it.size), plainStyle
	}
	return strings.Repeat(" ", sizeW), dimStyle
}

func nameStyle(it *item) lipgloss.Style {
	switch {
	case it.dir:
		return dirStyle
	case it.link:
		return linkStyle
	}
	return plainStyle
}

func (m *model) itemCell(it *item, w int, cur bool) string {
	sz, st := m.sizeField(it)
	name := fit(icon(it.name, it.dir, it.link)+" "+clean(it.name), w-sizeW-1)
	if cur {
		return m.cursorStyle(1).Render(sz + " " + name)
	}
	return st.Render(sz) + " " + nameStyle(it).Render(name)
}

// detailColumns returns the metadata columns that fit, leaving at least 20 cells for the name.
func (m *model) detailColumns() []detailCol {
	cols := slices.Clone(detailCols)
	for _, drop := range detailDropOrder {
		w := sizeW + 2 + 20
		for _, c := range cols {
			w += c.w + 2
		}
		if w <= m.contentW() {
			break
		}
		cols = slices.DeleteFunc(cols, func(c detailCol) bool { return c.title == drop })
	}
	return cols
}

// detailsView lists one entry per row under a fixed header row.
func (m *model) detailsView() string {
	iw, cols := m.contentW(), m.detailColumns()
	hdr := fmt.Sprintf("%*s", sizeW, "Size")
	for _, c := range cols {
		hdr += "  " + fit(c.title, c.w)
	}
	lines := []string{headerStyle.Render(fit(hdr+"  Name", iw))}
	if len(m.items) == 0 {
		lines = append(lines, dimStyle.Render("(empty)"))
	}
	for i := m.roff; i < len(m.items) && i < m.roff+m.innerH()-1; i++ {
		it, x := m.items[i], m.metaOf(m.items[i])
		sz, st := m.sizeField(it)
		mid := ""
		for _, c := range cols {
			mid += "  " + fit(clean(c.val(x)), c.w)
		}
		name := fit(icon(it.name, it.dir, it.link)+" "+clean(it.name), max(1, iw-sizeW-ansi.StringWidth(mid)-2))
		line := st.Render(sz) + mid + "  " + nameStyle(it).Render(name)
		if i == m.ccur {
			line = m.cursorStyle(1).Render(sz + mid + "  " + name)
		}
		lines = append(lines, ansi.Truncate(line, iw, ""))
	}
	return strings.Join(lines, "\n")
}

func (m *model) gridView() string {
	if len(m.items) == 0 {
		return dimStyle.Render("(empty)")
	}
	h, ws := m.innerH(), m.colWidths()
	lines := make([]string, h)
	for c := m.coff; c < m.coff+m.visibleCols(ws); c++ {
		w := ws[c]
		if c > m.coff {
			for r := range lines {
				lines[r] += strings.Repeat(" ", gap)
			}
		}
		for r := range lines {
			if i := c*h + r; i < len(m.items) {
				lines[r] += m.itemCell(m.items[i], w, i == m.ccur)
			} else {
				lines[r] += strings.Repeat(" ", w)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// visibleCols is how many whole grid columns fit in the contents pane starting at coff.
func (m *model) visibleCols(ws []int) int {
	used, n := 0, 0
	for c := m.coff; c < len(ws); c++ {
		if n > 0 && used+gap+ws[c] > m.contentW() {
			break
		}
		if n > 0 {
			used += gap
		}
		used += ws[c]
		n++
	}
	return n
}

func dialog(color, title, body string) string {
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(color)).
		Padding(1, 3).Render(lipgloss.NewStyle().Bold(true).Render(title) + "\n\n" + body)
}

func (m *model) confirmDialog() string {
	c := m.confirm
	return dialog("9", c.title, truncLeft(clean(c.path), max(10, m.w-12))+"\n"+
		dimStyle.Render(ansi.Truncate(clean(c.desc), max(10, m.w-12), "…"))+"\n\n"+
		warnStyle.Render("[y]")+" "+c.yes+"      "+warnStyle.Render("[n]")+" Cancel")
}

var helpKeys = [][2]string{
	{"↑ ↓ ← →", "Move"},
	{"PgUp PgDn", "Page up / down"},
	{"Home End", "First / last item"},
	{"Tab", "Switch pane"},
	{"Enter", "Open directory / view file"},
	{"Esc", "Parent directory"},
	{"g", "Go to path"},
	{"f", "Filter by glob pattern"},
	{"a", "Show all (clear filter)"},
	{"s", "Cycle sort order"},
	{"v", "Toggle grid / details view"},
	{"r", "Refresh sizes"},
	{"m", "Move / rename"},
	{"c", "Copy"},
	{"d", "Delete"},
	{"e", "Edit in $EDITOR"},
	{"h", "View file as hex"},
	{"x", "Execute (asks first)"},
	{"?", "This help"},
	{"q", "Quit"},
}

func (m *model) helpDialog() string {
	var lines []string
	for _, k := range helpKeys {
		lines = append(lines, warnStyle.Render(fit(k[0], 10))+"  "+fit(k[1], 32))
	}
	body := strings.Join(lines, "\n")
	if half := (len(lines) + 1) / 2; m.w >= 110 { // two columns when there's room
		body = lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(lines[:half], "\n"), "    ", strings.Join(lines[half:], "\n"))
	}
	return dialog("12", "Commands", body+"\n\n"+dimStyle.Render("Press any key to close"))
}

func (m *model) promptDialog() string {
	hint := "~ and relative paths work"
	switch m.prompt {
	case "filter":
		hint = "e.g. *.mp4 or *log*   (empty shows all)"
	case "move", "copy":
		hint = "Into an existing directory, or a new name. ~ and relative paths work"
	}
	return dialog("12", m.promptTitle, m.input.View()+"\n\n"+dimStyle.Render(hint)+"\n"+
		warnStyle.Render("[enter]")+" OK      "+warnStyle.Render("[esc]")+" Cancel")
}

// overlay draws fg centered on top of bg (both w cells wide).
func overlay(bg, fg string, w int) string {
	bl, fl := strings.Split(bg, "\n"), strings.Split(fg, "\n")
	x, y := max(0, (w-lipgloss.Width(fg))/2), max(0, (len(bl)-len(fl))/2)
	for i, l := range fl {
		if y+i < len(bl) {
			b := bl[y+i]
			bl[y+i] = ansi.Truncate(b, x, "") + "\x1b[0m" + l + ansi.TruncateLeft(b, x+ansi.StringWidth(l), "")
		}
	}
	return strings.Join(bl, "\n")
}

func (m *model) statusBar() string {
	keys := " ? help  enter open  esc up  f/a filter/all  s sort  v view  r refresh"
	done, total := 0, int64(0)
	for _, it := range m.items {
		if it.state == sized {
			done++
			total += it.size
		}
	}
	right, st := fmt.Sprintf("sort: %s   %d/%d sized   total %s ", sortNames[m.sortMode], done, len(m.items), strings.TrimSpace(fmtSize(total))), barStyle
	if m.busy != "" {
		right = m.spin.View() + " " + clean(m.busy) + " "
	}
	if m.err != "" {
		right, st = clean(m.err)+" ", errBarStyle
	}
	// Status info on the right wins; the key list is cut short if both don't fit.
	return st.Render(fit(fit(keys, max(0, m.w-ansi.StringWidth(right)-1))+" "+right, m.w))
}

func (m model) View() string {
	if m.w < 40 || m.h < 6 {
		return "Terminal too small."
	}
	if v := m.viewer; v != nil {
		keys := " ↑↓ scroll  ←→ pan  pgup/pgdn page  home/end top/bottom  q/esc close"
		pct := fmt.Sprintf("%3.0f%% ", v.vp.ScrollPercent()*100)
		bar := barStyle.Render(fit(fit(keys, m.w-len(pct)-1)+" "+pct, m.w))
		return box(v.title, v.vp.View(), m.w, m.innerH()+2, true) + "\n" + bar
	}
	h, tw := m.innerH(), m.treeW()
	rows := m.treeRows()
	var tl []string
	for i := m.toff; i < len(rows) && i < m.toff+h; i++ {
		tl = append(tl, m.treeLine(rows[i], i == m.tcur, tw-2))
	}
	left := box("Directories", strings.Join(tl, "\n"), tw, h+2, m.focus == 0)
	title := clean(m.dir)
	if m.filter != "" {
		title += "   filter: " + clean(m.filter)
	}
	body := m.gridView()
	if m.view == detailsView {
		body = m.detailsView()
	}
	right := box(title, body, m.w-tw, h+2, m.focus == 1)
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	switch {
	case m.confirm != nil:
		panes = overlay(panes, m.confirmDialog(), m.w)
	case m.prompt != "":
		panes = overlay(panes, m.promptDialog(), m.w)
	case m.help:
		panes = overlay(panes, m.helpDialog(), m.w)
	}
	return panes + "\n" + m.statusBar()
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m := model{
		focus: 1,
		start: cwd,
		root:  &node{name: "/", path: "/"},
		cache: map[string]sizeResult{},
		spin:  spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		input: textinput.New(),
	}
	m.input.Prompt = "> "
	m.input.Cursor.SetMode(cursor.CursorStatic) // no blink: avoids routing blink messages
	if !lipgloss.HasDarkBackground() {
		mdStyle = "light"
	}
	program = tea.NewProgram(m, tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
