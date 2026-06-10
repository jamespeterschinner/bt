package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fsnotify/fsnotify"
)

const defaultSearchHitLimit = 10

type Tree struct {
	Root       *Node
	CurrentDir *Node
	Marked     []*Node

	sortingFunc    NodeSortingFunc
	watcher        *fsnotify.Watcher
	flatNavigation bool
	inGitRepo      bool
	searchHitLimit int

	search        searchSnapshot
	searchMatches []SearchMatch
	searchCursor  int
}

// SearchMatch is one hit in the current search result list — a child at
// position Idx within Parent.Children.
type SearchMatch struct {
	Parent *Node
	Idx    int
}

func (m SearchMatch) Node() *Node {
	if m.Parent == nil || m.Idx < 0 || m.Idx >= len(m.Parent.Children) {
		return nil
	}
	return m.Parent.Children[m.Idx]
}

// searchSnapshot remembers state changed during an active incremental search
// so it can be reverted if the user cancels.
type searchSnapshot struct {
	active        bool
	originDir     *Node
	originIdx     int
	expandedNodes []*Node       // nodes whose Children were nil before search
	modifiedIdx   map[*Node]int // original selectedChildIdx of nodes the search changed
}

func (t *Tree) markIgnored(nodes []*Node) {
	if !t.inGitRepo || len(nodes) == 0 {
		return
	}
	paths := make([]string, len(nodes))
	for i, n := range nodes {
		paths[i] = n.Path
	}
	ignored := checkIgnored(t.Root.Path, paths)
	for _, n := range nodes {
		n.IsIgnored = ignored[n.Path]
	}
}

func (t *Tree) GetSelectedChild() *Node {
	if len(t.CurrentDir.Children) > 0 {
		return t.CurrentDir.Children[t.CurrentDir.selectedChildIdx]
	}
	return nil
}
func (t *Tree) ToggleHiddenInCurrentDirectory() error {
	t.CurrentDir.showHidden = !t.CurrentDir.showHidden
	if err := t.CurrentDir.readChildren(defaultNodeSorting); err != nil {
		return err
	}
	t.markIgnored(t.CurrentDir.Children)
	return nil
}
func (t *Tree) RemoveNodeFromMarkByPath(path string) {
	t.Marked = slices.DeleteFunc(
		t.Marked,
		func(n *Node) bool { return n.Path == path },
	)
}
func (t *Tree) RefreshNodeParentByPath(path string) error {
	parentDir := filepath.Dir(path)
	cur := t.Root
outer:
	for {
		// Reading children when a parent node found.
		if parentDir == cur.Path {
			if err := cur.readChildren(t.sortingFunc); err != nil {
				return err
			}
			t.markIgnored(cur.Children)
			return nil
		}
		// Going through directories towards `parentDir`.
		for _, ch := range cur.Children {
			if strings.HasPrefix(path, ch.Path) {
				cur = ch
				continue outer
			}
		}
		return nil
	}
}
func (t *Tree) RenameMarked(newName string) error {
	if len(t.Marked) != 1 {
		return nil
	}
	marked := t.Marked[0]

	if newName == "" {
		return fmt.Errorf("new name must not be empty")
	}
	targetPath := filepath.Join(marked.Parent.Path, newName)
	// NOTE: probably not the best way to check if file exists
	if _, err := os.Stat(targetPath); err == nil {
		return fmt.Errorf("file %s already exists", newName)
	} else if !os.IsNotExist(err) {
		return err
	}
	err := os.Rename(marked.Path, targetPath)
	if err != nil {
		return err
	}
	t.Marked = nil
	return nil
}
func (t *Tree) CreateFileInCurrent(name string) error {
	_, err := os.Create(filepath.Join(t.CurrentDir.Path, name))
	return err
}
func (t *Tree) CreateDirectoryInCurrent(name string) error {
	return os.Mkdir(filepath.Join(t.CurrentDir.Path, name), os.ModePerm)
}
func RecurseIntoNode(n *Node, index int, c *Node) (*Node, int) {
	lenChildren := len(c.Children)
	if lenChildren == 0 {
		return n, index
	} else {
		index := lenChildren - 1
		return RecurseIntoNode(c, index, c.Children[index])
	}
}
func RecurseOutOfNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	if n.selectedChildIdx < len(n.Children)-1 {
		return n
	} else {
		return RecurseOutOfNode(n.Parent)
	}
}
func (t *Tree) SelectPreviousChild() {
	if t.flatNavigation {
		if t.CurrentDir.selectedChildIdx > 0 {
			t.CurrentDir.selectedChildIdx -= 1
			index := t.CurrentDir.selectedChildIdx
			node := t.CurrentDir.Children[index]
			dir, idx := RecurseIntoNode(t.CurrentDir, index, node)
			t.CurrentDir = dir
			t.CurrentDir.selectedChildIdx = idx
		} else if t.CurrentDir.Parent != nil {
			t.CurrentDir = t.CurrentDir.Parent
		}
	} else {
		if t.CurrentDir.selectedChildIdx > 0 {
			t.CurrentDir.selectedChildIdx -= 1
		}
	}
}
func (t *Tree) SelectNextChild() {
	if t.flatNavigation {
		selected := t.GetSelectedChild()
		if selected != nil && len(selected.Children) > 0 {
			t.CurrentDir = selected
		} else if t.CurrentDir.selectedChildIdx < len(t.CurrentDir.Children)-1 {
			t.CurrentDir.selectedChildIdx += 1
		} else {
			dir := RecurseOutOfNode(t.CurrentDir.Parent)
			if dir != nil {
				t.CurrentDir = dir
				t.CurrentDir.selectedChildIdx += 1
			}
		}
	} else {
		if t.CurrentDir.selectedChildIdx < len(t.CurrentDir.Children)-1 {
			t.CurrentDir.selectedChildIdx += 1
		}
	}
}
func (t *Tree) SetSelectedChildAsCurrent() error {
	selectedChild := t.GetSelectedChild()
	if selectedChild == nil {
		return nil
	}
	if !selectedChild.Info.IsDir() {
		return nil
	}
	if selectedChild.Children == nil {
		err := selectedChild.readChildren(t.sortingFunc)
		if err != nil {
			return err
		}
		t.markIgnored(selectedChild.Children)
		t.watcher.Add(selectedChild.Path)
	}
	t.CurrentDir = selectedChild
	return nil
}
func (t *Tree) SetParentAsCurrent() {
	if t.CurrentDir.Parent != nil {
		currentName := t.CurrentDir.Info.Name()
		// setting parent current to match the directory, that we're leaving
		newParentIdx := slices.IndexFunc(t.CurrentDir.Parent.Children, func(n *Node) bool { return n.Info.Name() == currentName })
		t.CurrentDir.Parent.selectedChildIdx = newParentIdx

		t.CurrentDir = t.CurrentDir.Parent
	}
}
func (t *Tree) ToggleMarkSelectedChild() bool {
	if selected := t.GetSelectedChild(); selected != nil {
		if !slices.Contains(t.Marked, selected) {
			t.Marked = append(t.Marked, selected)
		} else {
			t.Marked = slices.DeleteFunc(t.Marked, func(n *Node) bool { return n == selected })
		}
		return true
	}
	return false
}
func (t *Tree) MarkSelectedChild() bool {
	if selected := t.GetSelectedChild(); selected != nil {
		if !slices.Contains(t.Marked, selected) {
			t.Marked = append(t.Marked, selected)
		}
		return true
	}
	return false
}
func (t *Tree) DropMark() {
	t.Marked = nil
}
func (t *Tree) DeleteMarked() error {
	if t.Marked == nil {
		return nil
	}
	for _, marked := range t.Marked {
		cmd := exec.Command("rm", "-r", marked.Path)
		err := cmd.Run()
		if err != nil {
			return err // todo: this is not the same error...?
		}
	}
	t.Marked = nil
	return nil
}
func (t *Tree) CopyMarkedToCurrentDir() error {
	if t.Marked == nil {
		return nil
	}
	targetDir := t.CurrentDir.Path
	for _, marked := range t.Marked {
		targetFileName, err := generateNewFileName(marked.Info.Name(), targetDir)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(targetDir, targetFileName)

		cmd := exec.Command("cp", "-r", marked.Path, targetPath)
		err = cmd.Run()
		if err != nil {
			return err // todo: this is not the same error...?
		}
	}
	t.Marked = nil
	return nil
}
func (t *Tree) MoveMarkedToCurrentDir() error {
	if t.Marked == nil {
		return nil
	}
	targetDir := t.CurrentDir.Path
	for _, marked := range t.Marked {
		targetFileName, err := generateNewFileName(marked.Info.Name(), targetDir)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(targetDir, targetFileName)

		cmd := exec.Command("mv", "-n", marked.Path, targetPath)
		err = cmd.Run()
		if err != nil {
			return err // todo: this is not the same error...?
		}
	}
	t.Marked = nil
	return nil
}
// BeginSearch snapshots the current position so a later RevertSearch can
// undo any tree state changes (expansions, selection moves) that Search
// makes as the user types.
func (t *Tree) BeginSearch() {
	t.search = searchSnapshot{
		active:      true,
		originDir:   t.CurrentDir,
		originIdx:   t.CurrentDir.selectedChildIdx,
		modifiedIdx: map[*Node]int{},
	}
	t.searchMatches = nil
	t.searchCursor = 0
}

// EndSearch commits the current search position and discards the snapshot.
func (t *Tree) EndSearch() {
	t.search = searchSnapshot{}
	t.searchMatches = nil
	t.searchCursor = 0
}

// RevertSearch restores the tree to the state it had when BeginSearch was
// called: selection indices are rolled back, directories expanded by the
// search are collapsed, and CurrentDir is reset to the origin.
func (t *Tree) RevertSearch() {
	if !t.search.active {
		return
	}
	for n, idx := range t.search.modifiedIdx {
		n.selectedChildIdx = idx
	}
	for i := len(t.search.expandedNodes) - 1; i >= 0; i-- {
		n := t.search.expandedNodes[i]
		n.orphanChildren()
		t.watcher.Remove(n.Path)
	}
	if t.search.originDir != nil {
		t.CurrentDir = t.search.originDir
		t.search.originDir.selectedChildIdx = t.search.originIdx
	}
	t.search = searchSnapshot{}
	t.searchMatches = nil
	t.searchCursor = 0
}

func (t *Tree) SearchMatches() []SearchMatch { return t.searchMatches }
func (t *Tree) SearchCursor() int            { return t.searchCursor }
func (t *Tree) SearchActive() bool           { return t.search.active }

// NextSearchHit moves the cursor to the next match and re-points CurrentDir.
func (t *Tree) NextSearchHit() {
	if len(t.searchMatches) == 0 {
		return
	}
	if t.searchCursor < len(t.searchMatches)-1 {
		t.searchCursor++
	}
	t.applySearchCursor()
}

// PrevSearchHit moves the cursor to the previous match and re-points CurrentDir.
func (t *Tree) PrevSearchHit() {
	if len(t.searchMatches) == 0 {
		return
	}
	if t.searchCursor > 0 {
		t.searchCursor--
	}
	t.applySearchCursor()
}

func (t *Tree) applySearchCursor() {
	m := t.searchMatches[t.searchCursor]
	if m.Parent == nil {
		return
	}
	t.recordIdxChange(m.Parent)
	t.CurrentDir = m.Parent
	m.Parent.selectedChildIdx = m.Idx
}

// SearchHitLimit returns the configured cap, falling back to the default.
func (t *Tree) SearchHitLimit() int {
	if t.searchHitLimit <= 0 {
		return defaultSearchHitLimit
	}
	return t.searchHitLimit
}

// FilenameBFS walks the real filesystem from root in level-order, returning
// up to `limit` paths whose basename contains `query` (case-insensitive).
// Pure I/O — safe to call from a tea.Cmd goroutine.
func FilenameBFS(query, root string, limit int) ([]string, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultSearchHitLimit
	}
	q := strings.ToLower(query)
	paths := make([]string, 0, limit)
	queue := []string{root}
	for len(queue) > 0 && len(paths) < limit {
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			if strings.Contains(strings.ToLower(name), q) {
				paths = append(paths, full)
				if len(paths) >= limit {
					break
				}
			}
			if e.IsDir() {
				queue = append(queue, full)
			}
		}
	}
	return paths, nil
}

func (t *Tree) recordIdxChange(n *Node) {
	if !t.search.active {
		return
	}
	if _, ok := t.search.modifiedIdx[n]; !ok {
		t.search.modifiedIdx[n] = n.selectedChildIdx
	}
}

// RipgrepFiles runs `rg -l` and returns the matching file paths. Pure I/O
// — safe to call from a tea.Cmd goroutine (does not touch Tree state).
// Returns (nil, nil) for an empty query or when rg found no matches.
func RipgrepFiles(query, root string) ([]string, error) {
	if query == "" {
		return nil, nil
	}
	cmd := exec.Command(
		"rg",
		"--files-with-matches",
		"--hidden",
		"--no-ignore",
		"-i",
		"--",
		query,
		root,
	)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("content search requires ripgrep (rg) on $PATH")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg: %w", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// ApplyPathMatches takes a list of file paths (from FilenameBFS or
// RipgrepFiles), walks the tree to each one (expanding into the snapshot),
// and populates searchMatches up to searchHitLimit. Empty paths / "" query
// revert CurrentDir to the search origin.
func (t *Tree) ApplyPathMatches(query string, paths []string) {
	t.searchCursor = 0
	if query == "" || len(paths) == 0 {
		t.searchMatches = nil
		if t.search.active && t.search.originDir != nil {
			t.CurrentDir = t.search.originDir
		}
		return
	}
	limit := t.searchHitLimit
	if limit <= 0 {
		limit = defaultSearchHitLimit
	}
	matches := make([]SearchMatch, 0, limit)
	for _, p := range paths {
		if p == "" {
			continue
		}
		parent, idx, err := t.locateByPath(p)
		if err != nil || parent == nil {
			continue
		}
		matches = append(matches, SearchMatch{Parent: parent, Idx: idx})
		if len(matches) >= limit {
			break
		}
	}
	t.searchMatches = matches
	if len(matches) == 0 {
		if t.search.active && t.search.originDir != nil {
			t.CurrentDir = t.search.originDir
		}
		return
	}
	t.applySearchCursor()
}

// locateByPath walks from Root toward absPath, expanding directories
// as needed (tracked in the search snapshot). Returns the matching
// child's parent and index, or (nil, 0, nil) if any segment is missing.
func (t *Tree) locateByPath(absPath string) (*Node, int, error) {
	rel, err := filepath.Rel(t.Root.Path, absPath)
	if err != nil {
		return nil, 0, err
	}
	segments := strings.Split(rel, string(filepath.Separator))
	if len(segments) == 0 || segments[0] == "." {
		return nil, 0, nil
	}
	cur := t.Root
	for i, seg := range segments {
		if cur.Info.IsDir() && cur.Children == nil {
			if err := cur.readChildren(t.sortingFunc); err != nil {
				return nil, 0, err
			}
			t.markIgnored(cur.Children)
			t.watcher.Add(cur.Path)
			if t.search.active {
				t.search.expandedNodes = append(t.search.expandedNodes, cur)
			}
		}
		found := -1
		for idx, ch := range cur.Children {
			if ch.Info.Name() == seg {
				found = idx
				break
			}
		}
		if found < 0 {
			return nil, 0, nil
		}
		if i == len(segments)-1 {
			return cur, found, nil
		}
		cur = cur.Children[found]
	}
	return nil, 0, nil
}

// RevealPath expands the directories along `target` (absolute, or relative to
// Root) so the node is visible, then selects it with CurrentDir pointing at its
// parent. Any miss — empty path, path outside Root, or a missing segment — is a
// silent no-op so callers can fall back to the default root view.
func (t *Tree) RevealPath(target string) error {
	if target == "" {
		return nil
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(t.Root.Path, abs)
	}
	parent, idx, err := t.locateByPath(abs)
	if err != nil || parent == nil {
		return nil
	}
	parent.selectedChildIdx = idx
	t.CurrentDir = parent
	return nil
}

func (t *Tree) CollapseOrExpandSelected() error {
	selectedChild := t.GetSelectedChild()
	if selectedChild == nil {
		return nil
	}
	if selectedChild.Children != nil {
		selectedChild.orphanChildren()
		t.watcher.Remove(selectedChild.Path)
	} else {
		err := selectedChild.readChildren(t.sortingFunc)
		if err != nil {
			return err
		}
		t.markIgnored(selectedChild.Children)
		t.watcher.Add(selectedChild.Path)
	}
	return nil
}

func InitTree(dir string, sortingFunc NodeSortingFunc, flatNavigation bool, searchHitLimit int) (*Tree, <-chan NodeChange, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}

	rootInfo, err := os.Lstat(absDir)
	if err != nil {
		return nil, nil, err
	}
	if !rootInfo.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", absDir)
	}
	if sortingFunc == nil {
		sortingFunc = defaultNodeSorting
	}

	root := NewNode(absDir, rootInfo, nil)

	err = root.readChildren(sortingFunc)
	if err != nil {
		return nil, nil, err
	}
	if len(root.Children) == 0 {
		return nil, nil, fmt.Errorf("Can't initialize on empty directory '%s'", absDir)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	changeChan := runFSWatcher(watcher)
	err = watcher.Add(root.Path)
	if err != nil {
		return nil, nil, err
	}

	tree := &Tree{
		Root:           root,
		CurrentDir:     root,
		sortingFunc:    sortingFunc,
		watcher:        watcher,
		flatNavigation: flatNavigation,
		inGitRepo:      detectGitRepo(absDir),
		searchHitLimit: searchHitLimit,
	}
	tree.markIgnored(root.Children)
	return tree, changeChan, nil
}

// Checks if fname already exists in targetDir.
// Adds "copy_" prefix (multiple times), until new file name becomes unique in derecotry.
func generateNewFileName(fname, targetDir string) (string, error) {
	currentDirContent, err := os.ReadDir(targetDir)
	if err != nil {
		return "", err
	}
	for slices.ContainsFunc(currentDirContent, func(e fs.DirEntry) bool { return e.Name() == fname }) {
		fname = "copy_" + fname
	}
	return fname, nil
}
