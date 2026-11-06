// Copyright (c) 2026 Andrew C. Young <andrew@vaelen.org>
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"sort"
	"testing"
)

func TestFmtSize(t *testing.T) {
	for in, want := range map[int64]string{
		0: "      0 B", 1023: "   1023 B", 1024: "   1.00 K", 1536: "   1.50 K",
		129446707: " 123.45 M", 3 << 29: "   1.50 G",
	} {
		if got := fmtSize(in); got != want || len(got) != sizeW {
			t.Errorf("fmtSize(%d) = %q; want %q", in, got, want)
		}
	}
}

func TestOrder(t *testing.T) {
	items := []*item{
		{name: "small", state: sized, size: 10},
		{name: "b", state: sizing},
		{name: "big", state: sized, size: 1000},
		{name: "a", state: queued},
	}
	for mode, want := range map[int]string{
		byLargest: "big small a b ", bySmallest: "small big a b ",
		byNameAsc: "a b big small ", byNameDesc: "small big b a ",
	} {
		sort.Slice(items, func(i, j int) bool { return less(items[i], items[j], mode) })
		got := ""
		for _, it := range items {
			got += it.name + " "
		}
		if got != want {
			t.Errorf("%s: order = %q; want %q", sortNames[mode], got, want)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"~": home, "~/Downloads": home + "/Downloads", "../b": "/x/b", "c/./d": "/x/a/c/d", "/etc": "/etc",
	} {
		if got := expandPath(in, "/x/a"); got != want {
			t.Errorf("expandPath(%q) = %q; want %q", in, got, want)
		}
	}
}
