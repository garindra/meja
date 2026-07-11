package terminal

import (
	"testing"
)

func TestFragmentedCSIAndCursorPositioning(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("abc\x1b[2"))
	term.Apply([]byte("D!"))
	if got := string([]rune{term.Cells[0].Rune, term.Cells[1].Rune, term.Cells[2].Rune}); got != "a!c" {
		t.Fatalf("got %q", got)
	}
}

func TestFragmentedUTF8(t *testing.T) {
	term := New(5, 1)
	term.Apply([]byte{0xe2, 0x82})
	term.Apply([]byte{0xac})
	if term.Cells[0].Rune != '€' {
		t.Fatalf("Rune = %q, want €", term.Cells[0].Rune)
	}
}

func TestCROverwrite(t *testing.T) {
	term := New(5, 1)
	term.Apply([]byte("10%\r30%"))
	got := string([]rune{term.Cells[0].Rune, term.Cells[1].Rune, term.Cells[2].Rune})
	if got != "30%" {
		t.Fatalf("got %q", got)
	}
}

func TestLFScrollAtBottom(t *testing.T) {
	term := New(3, 2)
	term.Apply([]byte("aaa\nbbb\nccc"))
	row0 := string([]rune{term.Cells[0].Rune, term.Cells[1].Rune, term.Cells[2].Rune})
	row1 := string([]rune{term.Cells[3].Rune, term.Cells[4].Rune, term.Cells[5].Rune})
	if row0 != "bbb" || row1 != "ccc" {
		t.Fatalf("rows = %q / %q", row0, row1)
	}
}

func TestEraseLine(t *testing.T) {
	term := New(5, 1)
	term.Apply([]byte("hello\x1b[3G\x1b[K"))
	got := string([]rune{term.Cells[0].Rune, term.Cells[1].Rune, term.Cells[2].Rune, term.Cells[3].Rune, term.Cells[4].Rune})
	if got != "he   " {
		t.Fatalf("got %q", got)
	}
}

func TestSGRAndReverseVideo(t *testing.T) {
	term := New(2, 1)
	update := term.Apply([]byte("\x1b[1;38;2;1;2;3;7mA"))
	if len(update.DefinedStyles) == 0 {
		t.Fatal("expected style definition")
	}
	cell := term.Cells[0]
	style := term.styleByID[cell.StyleID]
	if !style.Bold || !style.Reverse || style.FG.Mode != "rgb" || style.FG.R != 1 {
		t.Fatalf("style = %#v", style)
	}
}

func TestOSCIsConsumedAndNotPrinted(t *testing.T) {
	term := New(32, 1)
	term.Apply([]byte("\x1b]0;garindra@garindra-ubuntu: ~\x07garindra@garindra-ubuntu:~$ "))
	got := rowString(term, 0, 28)
	want := "garindra@garindra-ubuntu:~$ "
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOSCSTTerminatorIsConsumed(t *testing.T) {
	term := New(8, 1)
	term.Apply([]byte("\x1b]0;title\x1b\\prompt"))
	got := rowString(term, 0, 6)
	if got != "prompt" {
		t.Fatalf("got %q", got)
	}
}

func rowString(term *TerminalState, row, count int) string {
	runes := make([]rune, 0, count)
	for i := 0; i < count; i++ {
		r := term.Cells[row*term.Cols+i].Rune
		if r == 0 {
			r = ' '
		}
		runes = append(runes, r)
	}
	return string(runes)
}
