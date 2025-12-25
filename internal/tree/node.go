package tree

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type NodeSortingFunc func(a, b os.DirEntry) int

type Node struct {
	Index    int
	Path     string
	Info     fs.FileInfo
	Children []*Node // nil - not read or it's a file
	Parent   *Node

	UpNode           *Node
	DownNode         *Node
	selectedChildIdx int
	showHidden       bool
}

func (n *Node) SelectLast() {
	// can I just check for len here?
	if len(n.Children) > 0 {
		n.selectedChildIdx = len(n.Children) - 1
	}
}
func (n *Node) ShowsHidden() bool {
	return n.showHidden
}
func (n *Node) SelectFirst() {
	n.selectedChildIdx = 0
}
func (n *Node) readChildren(sortFunc NodeSortingFunc) error {
	if !n.Info.IsDir() {
		return nil
	}
	dirEntries, err := os.ReadDir(n.Path)
	if err != nil {
		return err
	}

	previousNode := n
	nDownNode := n.DownNode

	chNodes := []*Node{}

	slices.SortFunc(dirEntries, sortFunc)

	for index, dirEntry := range dirEntries {
		dirInfo, err := dirEntry.Info()
		if err != nil {
			return err
		}
		// Skipping hidden node
		if !n.showHidden && strings.HasPrefix(dirInfo.Name(), ".") {
			continue
		}
		// Looking if child already exist, so i'm keeping it's read children intact
		var childToAdd *Node
		if n.Children != nil {
			for _, ech := range n.Children {
				if ech.Info.Name() == dirInfo.Name() {
					childToAdd = ech
					childToAdd.Index = index
					childToAdd.Info = dirInfo // updating info in case file was changed
					childToAdd.UpNode = previousNode
					break
				}
			}
		}
		if childToAdd == nil {
			childToAdd = NewNode(
				index,
				filepath.Join(n.Path, dirInfo.Name()),
				dirInfo,
				n,
			)
			childToAdd.UpNode = previousNode

		}
		chNodes = append(chNodes, childToAdd)

		previousNode.DownNode = childToAdd
		previousNode = childToAdd
	}
	n.Children = chNodes

	if nDownNode != nil {
		nDownNode.UpNode = previousNode
		previousNode.DownNode = nDownNode
	}

	// updateing selected child index if it's out of bounds after update
	n.selectedChildIdx = max(min(n.selectedChildIdx, len(n.Children)-1), 0)
	return nil
}
func (n *Node) orphanChildren() {
	n.DownNode = nil

	// stitch the linked list back together
	lenChildren := len(n.Children)
	if lenChildren > 0 {
		lastChild := n.Children[lenChildren-1]
		if lastChild.DownNode != nil {
			n.DownNode = lastChild.DownNode
			n.DownNode.UpNode = n
		}
	}

	n.Children = nil
}
func (n *Node) ReadContent(buf []byte, limit int64) (int, error) {
	if !n.Info.Mode().IsRegular() {
		return 0, fmt.Errorf("file not selected or is irregular")
	}
	f, err := os.Open(n.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	limitedReader := io.LimitReader(f, limit)
	k, err := limitedReader.Read(buf)
	if err != nil {
		return 0, err
	}
	return k, nil
}

func NewNode(index int, path string, info fs.FileInfo, parent *Node) *Node {
	return &Node{
		Index:      index,
		Path:       path,
		Info:       info,
		Children:   nil,
		Parent:     parent,
		showHidden: true,
	}
}

func defaultNodeSorting(a, b os.DirEntry) int {
	// dirs first
	if a.IsDir() != b.IsDir() {
		if a.IsDir() {
			return -1
		} else {
			return 1
		}
	}
	return strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name()))
}
