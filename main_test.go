// Copyright (c) 2026 Andrew C. Young <andrew@vaelen.org>
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"sort"
	"strings"
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

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"ok.txt": "ok.txt", "a\x1b]0;pwned\x07b": "a?]0;pwned?b", "x\ny\tz": "x?y?z",
		"c1\u009b31m": "c1?31m", "raw\x9b": "raw\ufffd", "héllo 日本": "héllo 日本",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestIsBinary(t *testing.T) {
	long := strings.Repeat("a", 7999) + "é" // é straddles the 8000-byte sample boundary
	for in, want := range map[string]bool{
		"plain text\n": false, "héllo": false, long: false, "a\x00b": true, "\xff\xfe\x00": true, "bad \xff utf8": true,
	} {
		if got := isBinary([]byte(in)); got != want {
			t.Errorf("isBinary(%.20q) = %v; want %v", in, got, want)
		}
	}
}

func TestTextHelpers(t *testing.T) {
	if got, want := cleanText("a\r\nb\tc\x1b[31m"), "a\nb\tc?[31m"; got != want {
		t.Errorf("cleanText = %q; want %q", got, want)
	}
	if got, want := expandTabs("a\tb\n\tc"), "a       b\n        c"; got != want {
		t.Errorf("expandTabs = %q; want %q", got, want)
	}
}
