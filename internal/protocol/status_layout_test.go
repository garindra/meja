package protocol

import "testing"

func TestStatusTabAt(t *testing.T) {
	windows := []ClientStatusWindow{
		{Index: 0, Title: "zsh"},
		{Index: 1, Title: "vim", Active: true},
	}
	text, tabs := StatusText("main", 1, windows)
	if text != "[main] 0:zsh  1:vim* " {
		t.Fatalf("text = %q", text)
	}
	for _, test := range []struct {
		x     int
		index int
		ok    bool
	}{
		{0, 0, false}, {7, 0, true}, {12, 0, true}, {13, 0, false}, {14, 1, true}, {19, 1, true}, {20, 0, false},
	} {
		index, ok := StatusTabAt(80, test.x, text, "[host:~]", tabs)
		if ok != test.ok || (ok && index != test.index) {
			t.Errorf("x=%d got (%d,%v) want (%d,%v)", test.x, index, ok, test.index, test.ok)
		}
	}
	if _, ok := StatusTabAt(10, 7, text, "[host:~]", tabs); ok {
		t.Errorf("clicked truncated tab")
	}
}
