package terminal

import (
	"bytes"
	"strings"
	"testing"

	"tali/internal/protocol"
)

func TestCanonicalDefaultStyleOwnsIDZero(t *testing.T) {
	term := New(4, 1)
	if term.styleByID[protocol.CanonicalDefaultStyleID] != protocol.CanonicalDefaultStyle() {
		t.Fatalf("style 0=%#v, want canonical default", term.styleByID[protocol.CanonicalDefaultStyleID])
	}
	term.CurrentStyle = Style{Bold: true}
	term.Apply([]byte("x"))
	if term.Cells[0].StyleID == protocol.CanonicalDefaultStyleID {
		t.Fatal("dynamic style reused canonical style ID 0")
	}
}

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

func TestChineseRuneOccupiesTwoCells(t *testing.T) {
	term := New(5, 1)
	encoded := []byte("界")
	term.Apply(encoded[:1])
	term.Apply(encoded[1:2])
	update := term.Apply(encoded[2:])

	if got := term.GridRows[0].Cells[0]; got.Rune != '界' || got.Width != 2 {
		t.Fatalf("leading cell = %#v, want width-two 界", got)
	}
	if got := term.GridRows[0].Cells[1]; got.Width != 0 {
		t.Fatalf("continuation cell = %#v, want width zero", got)
	}
	if term.CursorX != 2 {
		t.Fatalf("cursor column = %d, want 2", term.CursorX)
	}
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 0, End: 2}); got != want {
		t.Fatalf("dirty span = %#v, want %#v", got, want)
	}
}

func TestChineseRuneWrapsBeforeLastColumn(t *testing.T) {
	term := New(4, 2)
	term.Apply([]byte("abc界"))

	if term.GridRows[1].Cells[0].Rune != '界' || term.GridRows[1].Cells[0].Width != 2 {
		t.Fatalf("wrapped row = %#v", term.GridRows[1].Cells)
	}
	if term.CursorY != 1 || term.CursorX != 2 {
		t.Fatalf("cursor = %d,%d, want 2,1", term.CursorX, term.CursorY)
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

func TestFullPrimaryScrollCapturesBoundedHistory(t *testing.T) {
	term := New(3, 2)
	term.HistoryLimit = 2
	term.Apply([]byte("aaa\nbbb\nccc\nddd"))
	if len(term.History) != 2 {
		t.Fatalf("history rows = %d, want 2", len(term.History))
	}
	if got := cellsString(term.History[0].Cells); got != "aaa" {
		t.Fatalf("oldest retained history row = %q, want aaa", got)
	}
	if got := cellsString(term.History[1].Cells); got != "bbb" {
		t.Fatalf("newest retained history row = %q, want bbb", got)
	}
}

func TestPartialScrollRegionDoesNotAppendHistory(t *testing.T) {
	term := New(4, 4)
	term.Apply([]byte("1111\n2222\n3333\n4444"))
	before := len(term.History)
	term.Apply([]byte("\x1b[2;3r"))
	term.CursorY = 2
	term.Apply([]byte("\n"))
	if len(term.History) != before {
		t.Fatalf("partial-region scroll appended history: before=%d after=%d", before, len(term.History))
	}
}

func TestLFWithoutScrollDoesNotForceFullRedraw(t *testing.T) {
	term := New(3, 2)
	update := term.Apply([]byte("a\n"))
	if update.FullRedraw {
		t.Fatal("newline without scroll forced full redraw")
	}
}

func TestFullViewportLineFeedReportsScrollAndNewRowDamage(t *testing.T) {
	term := New(3, 2)
	term.Apply([]byte("aaa\r\nbbb"))

	update := term.Apply([]byte("\r\nccc"))

	if update.FullRedraw {
		t.Fatal("full viewport scroll forced full redraw")
	}
	if update.ScrollDelta != -1 {
		t.Fatalf("scroll delta = %d, want -1", update.ScrollDelta)
	}
	if len(update.DirtySpans) != 1 {
		t.Fatalf("dirty spans = %#v, want only newly exposed row", update.DirtySpans)
	}
	if got, want := update.DirtySpans[1], (DirtySpan{Start: 0, End: 3}); got != want {
		t.Fatalf("bottom row damage = %#v, want %#v", got, want)
	}
}

func TestPartialRegionLineFeedStillRequiresFullRedraw(t *testing.T) {
	term := New(4, 4)
	term.Apply([]byte("\x1b[2;3r\x1b[3;1H"))

	update := term.Apply([]byte("\n"))

	if !update.FullRedraw || update.ScrollDelta != 0 {
		t.Fatalf("partial scroll update = %#v, want full redraw without viewport scroll", update)
	}
}

func TestPrintableOutputTracksDirtyColumnSpan(t *testing.T) {
	term := New(10, 1)
	term.CursorX = 4
	update := term.Apply([]byte("l"))
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 4, End: 5}); got != want {
		t.Fatalf("dirty span = %#v, want %#v", got, want)
	}
}

func TestPrintableOutputMergesDirtyColumnSpans(t *testing.T) {
	term := New(10, 1)
	update := term.Apply([]byte("abc"))
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 0, End: 3}); got != want {
		t.Fatalf("dirty span = %#v, want %#v", got, want)
	}
}

func TestEraseLine(t *testing.T) {
	term := New(5, 1)
	term.Apply([]byte("hello"))
	update := term.Apply([]byte("\x1b[3G\x1b[K"))
	got := string([]rune{term.Cells[0].Rune, term.Cells[1].Rune, term.Cells[2].Rune, term.Cells[3].Rune, term.Cells[4].Rune})
	if got != "he   " {
		t.Fatalf("got %q", got)
	}
	if update.FullRedraw {
		t.Fatal("erase line forced a full redraw")
	}
	if _, ok := update.DirtyRows[0]; !ok {
		t.Fatal("erase line did not mark its row dirty")
	}
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 2, End: 5}); got != want {
		t.Fatalf("erase line dirty span = %#v, want %#v", got, want)
	}
}

func TestClearErasesEntireCanonicalGrid(t *testing.T) {
	term := New(8, 3)
	term.Apply([]byte("first\nsecond\nthird"))
	update := term.Apply([]byte("\x1b[H\x1b[2J\x1b[3J"))
	if !update.FullRedraw {
		t.Fatal("clear did not request full redraw")
	}
	for i, cell := range term.Cells {
		if cell.Rune != ' ' {
			t.Fatalf("cell %d survived clear: %#v", i, cell)
		}
	}
}

func TestAlternateScreenRestoresPrimaryGrid(t *testing.T) {
	term := New(8, 3)
	term.Apply([]byte("prompt"))
	term.Apply([]byte("\x1b[?1049hTUI data"))
	if !term.Alternate {
		t.Fatal("alternate screen not entered")
	}
	term.Apply([]byte("\nmore\nrows\nscroll"))
	if len(term.History) != 0 {
		t.Fatalf("alternate output entered primary history: %d", len(term.History))
	}
	update := term.Apply([]byte("\x1b[?1049l"))
	if !update.FullRedraw || term.Alternate {
		t.Fatal("alternate screen not exited")
	}
	if got := cellsString(term.GridRows[0].Cells[:6]); got != "prompt" {
		t.Fatalf("restored primary row=%q", got)
	}
}

func TestAlternateResizePreservesPrimary(t *testing.T) {
	term := New(8, 3)
	term.Apply([]byte("primary"))
	term.Apply([]byte("\x1b[?1049hTUI"))
	term.Resize(12, 4)
	term.Apply([]byte("\x1b[?1049l"))
	if got := cellsString(term.GridRows[0].Cells[:7]); got != "primary" {
		t.Fatalf("primary after alternate resize=%q", got)
	}
}

func TestApplicationCursorMode(t *testing.T) {
	term := New(8, 3)
	term.Apply([]byte("\x1b[?1h"))
	if !term.ApplicationCursorKeys {
		t.Fatal("application cursor mode not enabled")
	}
	term.Resize(10, 4)
	if !term.ApplicationCursorKeys {
		t.Fatal("resize lost application cursor mode")
	}
	term.Apply([]byte("\x1b[?1l"))
	if term.ApplicationCursorKeys {
		t.Fatal("application cursor mode not disabled")
	}
}

func TestEraseCharsMarksOnlyCurrentRowDirty(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("hello"))
	term.CursorX = 1
	update := term.Apply([]byte("\x1b[2X"))
	if update.FullRedraw {
		t.Fatal("erase characters forced a full redraw")
	}
	if len(update.DirtyRows) != 1 {
		t.Fatalf("erase characters dirty rows = %#v", update.DirtyRows)
	}
	if _, ok := update.DirtyRows[0]; !ok {
		t.Fatal("erase characters did not mark its row dirty")
	}
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 1, End: 3}); got != want {
		t.Fatalf("erase characters dirty span = %#v, want %#v", got, want)
	}
}

func TestInsertCharactersShiftsExistingTextRight(t *testing.T) {
	term := New(12, 1)
	term.Apply([]byte("abcdef\r\x1b[3C\x1b[3@zzz"))
	if got := cellsString(term.GridRows[0].Cells[:9]); got != "abczzzdef" {
		t.Fatalf("inserted text=%q", got)
	}
}

func TestRepeatPrecedingCharacterPreservesStyle(t *testing.T) {
	term := New(8, 1)
	update := term.Apply([]byte("\x1b[44m \x1b[6b"))
	if update.FullRedraw {
		t.Fatal("REP forced full redraw")
	}
	for column := 0; column < 7; column++ {
		cell := term.GridRows[0].Cells[column]
		if cell.Rune != ' ' || term.styleByID[cell.StyleID].BG.Index != 4 {
			t.Fatalf("column %d=%#v style=%#v", column, cell, term.styleByID[cell.StyleID])
		}
	}
}

func TestDeleteCharactersUsesCurrentBackground(t *testing.T) {
	term := New(8, 1)
	term.Apply([]byte("abcdefgh\r\x1b[44m\x1b[3P"))
	if got := cellsString(term.GridRows[0].Cells); got != "defgh   " {
		t.Fatalf("DCH row=%q", got)
	}
	for column := 5; column < 8; column++ {
		style := term.styleByID[term.GridRows[0].Cells[column].StyleID]
		if style.BG.Mode != "indexed" || style.BG.Index != 4 {
			t.Fatalf("column %d background=%#v", column, style.BG)
		}
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

func TestCharsetDesignationIsConsumedAndNotPrinted(t *testing.T) {
	term := New(8, 1)
	term.Apply([]byte("\x1b(Bprompt"))
	got := rowString(term, 0, 6)
	if got != "prompt" {
		t.Fatalf("got %q", got)
	}
}

func TestResizePreservesVisibleContent(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("hello\nworld"))

	term.Resize(5, 2)
	if got := rowString(term, 0, 5); got != "hello" {
		t.Fatalf("row 0 after same-size resize = %q", got)
	}
	if got := rowString(term, 1, 5); got != "world" {
		t.Fatalf("row 1 after same-size resize = %q", got)
	}

	term.Resize(7, 3)
	if got := rowString(term, 0, 5); got != "hello" {
		t.Fatalf("row 0 after grow resize = %q", got)
	}
	if got := rowString(term, 1, 5); got != "world" {
		t.Fatalf("row 1 after grow resize = %q", got)
	}
}

func TestResizeShrinksByReflowingRows(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("abcde\nvwxyz"))

	term.Resize(3, 4)
	if got := rowString(term, 0, 3); got != "abc" {
		t.Fatalf("row 0 after shrink resize = %q", got)
	}
	if got := rowString(term, 1, 3); got != "de " {
		t.Fatalf("row 1 after shrink resize = %q", got)
	}
	if got := rowString(term, 2, 3); got != "vwx" {
		t.Fatalf("row 2 after shrink resize = %q", got)
	}
	if got := rowString(term, 3, 3); got != "yz " {
		t.Fatalf("row 3 after shrink resize = %q", got)
	}
}

func TestResizeShrinkKeepsBottomContentVisible(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("hello\nworld"))

	term.Resize(3, 2)
	if got := rowString(term, 0, 3); got != "wor" {
		t.Fatalf("row 0 after bottom-preserving shrink resize = %q", got)
	}
	if got := rowString(term, 1, 3); got != "ld " {
		t.Fatalf("row 1 after bottom-preserving shrink resize = %q", got)
	}
}

func TestAutoWrapMarksWrapsNext(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("abcdef"))
	if !term.GridRows[0].WrapsNext {
		t.Fatal("row 0 should wrap into row 1")
	}
	if term.GridRows[1].WrapsNext {
		t.Fatal("row 1 should not wrap onward")
	}
}

func TestLFCreatesHardBoundary(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("abc\n"))
	if term.GridRows[0].WrapsNext {
		t.Fatal("newline should not leave soft-wrap metadata behind")
	}
}

func TestCRDoesNotCreateFalseWrapChain(t *testing.T) {
	term := New(5, 2)
	term.Apply([]byte("abcdef"))
	if !term.GridRows[0].WrapsNext {
		t.Fatal("expected initial soft wrap")
	}
	term.Apply([]byte("\rZ"))
	if term.GridRows[0].WrapsNext {
		t.Fatal("carriage-return overwrite should conservatively break the wrap chain")
	}
}

func TestResizeShrinkAndGrowRestoresSoftWrappedChain(t *testing.T) {
	term := New(8, 2)
	term.Apply([]byte("abcdefghijkl"))
	term.Resize(4, 4)
	if got := rowString(term, 0, 4); got != "abcd" {
		t.Fatalf("row 0 after shrink = %q", got)
	}
	if got := rowString(term, 2, 4); got != "ijkl" {
		t.Fatalf("row 2 after shrink = %q", got)
	}

	term.Resize(8, 2)
	if got := rowString(term, 0, 8); got != "abcdefgh" {
		t.Fatalf("row 0 after grow = %q", got)
	}
	if got := rowString(term, 1, 8); got != "ijkl    " {
		t.Fatalf("row 1 after grow = %q", got)
	}
}

func TestUnsupportedCSIIsLogged(t *testing.T) {
	var buf bytes.Buffer
	SetDebugLogger(&buf)
	defer SetDebugLogger(nil)

	term := New(5, 1)
	term.Apply([]byte("\x1b[1L"))

	if got := buf.String(); !strings.Contains(got, "unsupported CSI 1L") {
		t.Fatalf("debug log = %q", got)
	}
}

func TestSaveRestoreCursorAndStyle(t *testing.T) {
	term := New(5, 2)
	term.CursorX = 2
	term.CursorY = 1
	term.CurrentStyle = Style{Bold: true}
	term.Apply([]byte("\x1b7"))
	term.CursorX = 0
	term.CursorY = 0
	term.CurrentStyle = DefaultStyle
	term.Apply([]byte("\x1b8"))
	if term.CursorX != 2 || term.CursorY != 1 || !term.CurrentStyle.Bold {
		t.Fatalf("restored cursor/style = (%d,%d) %#v", term.CursorX, term.CursorY, term.CurrentStyle)
	}
}

func TestDeleteChars(t *testing.T) {
	term := New(5, 1)
	term.Apply([]byte("abcde"))
	term.CursorX = 1
	term.Apply([]byte("\x1b[1P"))
	if got := rowString(term, 0, 5); got != "acde " {
		t.Fatalf("row after DCH = %q", got)
	}
}

func TestScrollRegionLineFeedScrollsWithinMargins(t *testing.T) {
	term := New(4, 4)
	term.Apply([]byte("1111\n2222\n3333\n4444"))
	term.Apply([]byte("\x1b[2;3r"))
	term.CursorY = 2
	term.CursorX = 0
	term.Apply([]byte("\n"))
	if got := rowString(term, 0, 4); got != "1111" {
		t.Fatalf("row 0 changed outside margins: %q", got)
	}
	if got := rowString(term, 1, 4); got != "3333" {
		t.Fatalf("row 1 after margin scroll = %q", got)
	}
	if got := rowString(term, 2, 4); got != "    " {
		t.Fatalf("row 2 after margin scroll = %q", got)
	}
}

func TestReverseIndexAtTopMarginScrollsDown(t *testing.T) {
	term := New(4, 4)
	term.Apply([]byte("1111\n2222\n3333\n4444"))
	term.Apply([]byte("\x1b[2;3r"))
	term.CursorY = 1
	term.Apply([]byte("\x1bM"))
	if got := rowString(term, 1, 4); got != "    " {
		t.Fatalf("top margin row after RI = %q", got)
	}
	if got := rowString(term, 2, 4); got != "2222" {
		t.Fatalf("shifted row after RI = %q", got)
	}
}

func TestDSRReply(t *testing.T) {
	term := New(5, 2)
	term.CursorX = 2
	term.CursorY = 1
	update := term.Apply([]byte("\x1b[6n"))
	if len(update.Replies) != 1 || string(update.Replies[0]) != "\x1b[2;3R" {
		t.Fatalf("DSR replies = %#v", update.Replies)
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

func cellsString(cells []Cell) string {
	runes := make([]rune, len(cells))
	for i, cell := range cells {
		runes[i] = cell.Rune
		if runes[i] == 0 {
			runes[i] = ' '
		}
	}
	return string(runes)
}
