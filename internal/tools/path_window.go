package tools

import (
	"container/heap"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type pathHeap []string

func (h pathHeap) Len() int           { return len(h) }
func (h pathHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h pathHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pathHeap) Push(v any)        { *h = append(*h, v.(string)) }
func (h *pathHeap) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

// Keep only the first n lexical paths while still counting scanned matches.
type pathWindow struct {
	paths        pathHeap
	limit, total int
}

func (w *pathWindow) add(path string) {
	w.total++
	if len(w.paths) < w.limit {
		heap.Push(&w.paths, path)
		return
	}
	if w.limit > 0 && path < w.paths[0] {
		w.paths[0] = path
		heap.Fix(&w.paths, 0)
	}
}
func (w *pathWindow) sorted() []string {
	paths := append([]string(nil), w.paths...)
	sort.Strings(paths)
	return paths
}
func displayPath(path string) string {
	if !utf8.ValidString(path) || strings.TrimSpace(path) != path {
		return strconv.Quote(path)
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return strconv.Quote(path)
		}
	}
	return path
}
