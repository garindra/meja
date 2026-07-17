package server

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestCanonicalDefaultStyleOwnsIDZero(t *testing.T) {
	term := newTerminal(4, 1)
	if term.styleByID[protocol.CanonicalDefaultStyleID] != protocol.CanonicalDefaultStyle() {
		t.Fatalf("style 0=%#v, want canonical default", term.styleByID[protocol.CanonicalDefaultStyleID])
	}
	term.CurrentStyle = Style{Bold: true}
	term.Apply([]byte("x"))
	if term.GridRows[0].Cells[0].StyleID == protocol.CanonicalDefaultStyleID {
		t.Fatal("dynamic style reused canonical style ID 0")
	}
}

func TestFragmentedCSIAndCursorPositioning(t *testing.T) {
	term := newTerminal(5, 2)
	term.Apply([]byte("abc\x1b[2"))
	term.Apply([]byte("D!"))
	if got := rowString(term, 0, 3); got != "a!c" {
		t.Fatalf("got %q", got)
	}
}

func TestResizePreservesIncrementalParserAndSavedCursor(t *testing.T) {
	term := newTerminal(5, 2)
	term.CursorX, term.CursorY = 4, 1
	term.Apply([]byte("\x1b7\x1b[2"))
	term.Resize(8, 3)
	term.Apply([]byte("D!\x1b8"))
	if term.GridRows[1].Cells[0].Cluster != "!" || term.CursorX != 4 || term.CursorY != 1 {
		t.Fatalf("resized parser/cursor state cursor=%d,%d row=%q", term.CursorX, term.CursorY, rowString(term, 1, 8))
	}
}

func TestFragmentedUTF8(t *testing.T) {
	term := newTerminal(5, 1)
	term.Apply([]byte{0xe2, 0x82})
	term.Apply([]byte{0xac})
	if term.GridRows[0].Cells[0].Cluster != "€" {
		t.Fatalf("Cluster = %q, want €", term.GridRows[0].Cells[0].Cluster)
	}
}

func TestGraphemeClustersHaveCanonicalCellsAndWidths(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		width   uint8
	}{
		{name: "combining accent", cluster: "e\u0301", width: 1},
		{name: "text heart", cluster: "❤", width: 1},
		{name: "emoji heart", cluster: "❤️", width: 2},
		{name: "skin tone", cluster: "👍🏽", width: 2},
		{name: "zwj", cluster: "👩‍💻", width: 2},
		{name: "keycap", cluster: "1️⃣", width: 2},
		{name: "multiple marks", cluster: "e\u0301\u0323", width: 1},
		{name: "latin stacked marks", cluster: "a\u030a\u0301", width: 1},
		{name: "hebrew points", cluster: "שָׁ", width: 1},
		{name: "arabic marks", cluster: "نَّ", width: 1},
		{name: "thai tone mark", cluster: "ก้", width: 1},
		{name: "tamil vowel sign", cluster: "நி", width: 1},
		{name: "devanagari conjunct", cluster: "क्ष", width: 2},
		{name: "hangul jamo", cluster: "각", width: 2},
		{name: "cjk ideographic variation", cluster: "葛\U000e0100", width: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term := newTerminal(8, 2)
			term.Apply([]byte(tt.cluster + "X"))
			anchor := term.GridRows[0].Cells[0]
			if anchor.Cluster != tt.cluster || anchor.Width != tt.width {
				t.Fatalf("anchor=%#v, want cluster %q width %d", anchor, tt.cluster, tt.width)
			}
			if tt.width == 2 && term.GridRows[0].Cells[1].Width != 0 {
				t.Fatalf("continuation=%#v", term.GridRows[0].Cells[1])
			}
			if got := term.GridRows[0].Cells[tt.width].Cluster; got != "X" {
				t.Fatalf("following cell=%q, want X", got)
			}
			if term.CursorX != int(tt.width)+1 {
				t.Fatalf("cursor=%d, want %d", term.CursorX, tt.width+1)
			}
		})
	}
}

func TestGraphemeChunkBoundariesDoNotChangeFinalState(t *testing.T) {
	sequences := []string{
		"e\u0301\u0323", "שָׁ", "نَّ", "ก้", "நி", "क्ष", "각", "葛\U000e0100",
		"❤️", "👍🏽", "👩‍💻", "1️⃣",
	}
	for _, sequence := range sequences {
		want := newTerminal(8, 2)
		want.Apply([]byte(sequence))
		encoded := []byte(sequence)
		for split := 1; split < len(encoded); split++ {
			got := newTerminal(8, 2)
			got.Apply(encoded[:split])
			got.Apply(encoded[split:])
			if !reflect.DeepEqual(got.GridRows, want.GridRows) || got.CursorX != want.CursorX || got.CursorY != want.CursorY || got.wrapPending != want.wrapPending {
				t.Fatalf("%q split %d: grid=%#v cursor=%d,%d wrap=%v; want grid=%#v cursor=%d,%d wrap=%v", sequence, split, got.GridRows, got.CursorX, got.CursorY, got.wrapPending, want.GridRows, want.CursorX, want.CursorY, want.wrapPending)
			}
		}
	}
}

func TestGraphemeBytewiseAndScalarwiseStreamingMatchesAtomicInput(t *testing.T) {
	sequences := []string{
		"a\u030a\u0301", "שָׁ", "نَّ", "क्ष", "葛\U000e0100",
		"1️⃣", "🇮🇳", "👩‍💻",
	}
	for _, sequence := range sequences {
		t.Run(sequence, func(t *testing.T) {
			want := newTerminal(12, 2)
			want.Apply([]byte(sequence + "X"))

			bytewise := newTerminal(12, 2)
			for _, b := range []byte(sequence + "X") {
				bytewise.Apply([]byte{b})
			}
			if !reflect.DeepEqual(bytewise.GridRows, want.GridRows) || bytewise.CursorX != want.CursorX {
				t.Fatalf("bytewise state differs: grid=%#v cursor=%d; want grid=%#v cursor=%d", bytewise.GridRows, bytewise.CursorX, want.GridRows, want.CursorX)
			}

			scalarwise := newTerminal(12, 2)
			for _, r := range sequence {
				scalarwise.Apply([]byte(string(r)))
			}
			scalarwise.Apply([]byte("X"))
			if !reflect.DeepEqual(scalarwise.GridRows, want.GridRows) || scalarwise.CursorX != want.CursorX {
				t.Fatalf("scalarwise state differs: grid=%#v cursor=%d; want grid=%#v cursor=%d", scalarwise.GridRows, scalarwise.CursorX, want.GridRows, want.CursorX)
			}
		})
	}
}

func TestBulkASCIILeavesOnlyItsFinalCellExtendable(t *testing.T) {
	t.Run("combining mark", func(t *testing.T) {
		term := newTerminal(12, 1)
		term.Apply([]byte("fast-e"))
		term.Apply([]byte("\u0301"))
		if got := term.GridRows[0].Cells[5]; got.Cluster != "e\u0301" || got.Width != 1 {
			t.Fatalf("extended final ASCII cell = %#v", got)
		}
		if term.CursorX != 6 {
			t.Fatalf("cursor after combining extension = %d, want 6", term.CursorX)
		}
	})

	t.Run("keycap promotion", func(t *testing.T) {
		term := newTerminal(12, 1)
		term.Apply([]byte("fast1"))
		term.Apply([]byte("\ufe0f\u20e3"))
		if got := term.GridRows[0].Cells[4]; got.Cluster != "1️⃣" || got.Width != 2 {
			t.Fatalf("promoted final ASCII cell = %#v", got)
		}
		if continuation := term.GridRows[0].Cells[5]; continuation.Width != 0 {
			t.Fatalf("keycap continuation = %#v", continuation)
		}
	})

	t.Run("trailing blank closes tail", func(t *testing.T) {
		term := newTerminal(12, 1)
		term.Apply([]byte("e "))
		term.Apply([]byte("\u0301"))
		if got := term.GridRows[0].Cells[0]; got.Cluster != "e" {
			t.Fatalf("combining mark crossed trailing blank: %#v", got)
		}
		if got := term.GridRows[0].Cells[2]; got.Cluster != "\u0301" || got.Width != 1 {
			t.Fatalf("leading-mark fallback = %#v", got)
		}
	})
}

func TestBulkASCIIOverwriteRepairsWideClusterEdges(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("界界界"))
	term.Apply([]byte("\x1b[2Gab"))
	want := []Cell{
		blankCell(0),
		{Cluster: "a", Width: 1},
		{Cluster: "b", Width: 1},
		blankCell(0),
		{Cluster: "界", Width: 2},
		{Width: 0},
	}
	if got := term.GridRows[0].Cells[:len(want)]; !reflect.DeepEqual(got, want) {
		t.Fatalf("cells after edge overwrite = %#v, want %#v", got, want)
	}
}

func TestInternationalWidthTwoClustersAreAtomicWhenContinuationIsOverwritten(t *testing.T) {
	for _, cluster := range []string{"क्ष", "葛\U000e0100", "👩‍💻"} {
		t.Run(cluster, func(t *testing.T) {
			term := newTerminal(6, 1)
			term.Apply([]byte("A" + cluster + "Z"))
			term.Apply([]byte("\x1b[1;3HQ"))
			if oldAnchor := term.GridRows[0].Cells[1]; oldAnchor.Cluster != "" || oldAnchor.Width != 1 {
				t.Fatalf("old anchor was not atomically cleared: %#v", oldAnchor)
			}
			if replacement := term.GridRows[0].Cells[2]; replacement.Cluster != "Q" || replacement.Width != 1 {
				t.Fatalf("continuation replacement = %#v", replacement)
			}
			if following := term.GridRows[0].Cells[3]; following.Cluster != "Z" {
				t.Fatalf("following cell was damaged: %#v", following)
			}
		})
	}
}

func TestIndicViramaFallbackDoesNotJoinAnotherScript(t *testing.T) {
	term := newTerminal(6, 1)
	term.Apply([]byte("क्A"))
	if got := term.GridRows[0].Cells[0]; got.Cluster != "क्" || got.Width != 1 {
		t.Fatalf("Indic anchor = %#v", got)
	}
	if got := term.GridRows[0].Cells[1]; got.Cluster != "A" || got.Width != 1 {
		t.Fatalf("following Latin cell = %#v", got)
	}
}

func TestReadlineBackspaceDeletesBedThenTeddyBearOneClusterAtATime(t *testing.T) {
	const prompt = "P> "
	tests := []struct {
		name                      string
		first, second             string
		firstWidth, secondWidth   int
		deleteSecond, deleteFirst string
	}{
		{name: "teddy then bed", first: "🧸", second: "🛏️", firstWidth: 2, secondWidth: 2, deleteSecond: "\b\x1b[K", deleteFirst: "\b\b\x1b[K"},
		{name: "cjk then airplane", first: "界", second: "✈️", firstWidth: 2, secondWidth: 2, deleteSecond: "\b\x1b[K", deleteFirst: "\b\b\x1b[K"},
		{name: "two promoted emoji", first: "☀️", second: "⚙️", firstWidth: 2, secondWidth: 2, deleteSecond: "\b\x1b[K", deleteFirst: "\b\x1b[K"},
		{name: "ascii then keycap without vs16", first: "A", second: "1\u20e3", firstWidth: 1, secondWidth: 2, deleteSecond: "\b\x1b[K", deleteFirst: "\b\x1b[K"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term := newTerminal(20, 1)
			term.Apply([]byte(prompt + tt.first + tt.second))
			start := len(prompt)
			if term.CursorX != start+tt.firstWidth+tt.secondWidth {
				t.Fatalf("cursor after input = %d", term.CursorX)
			}

			term.Apply([]byte(tt.deleteSecond))
			if term.CursorX != start+tt.firstWidth {
				t.Fatalf("cursor after deleting second cluster = %d, want %d", term.CursorX, start+tt.firstWidth)
			}
			if first := term.GridRows[0].Cells[start]; first.Cluster != tt.first || int(first.Width) != tt.firstWidth {
				t.Fatalf("first cluster after deletion = %#v", first)
			}
			if second := term.GridRows[0].Cells[start+tt.firstWidth]; second.Cluster != "" || second.Width != 1 {
				t.Fatalf("second cluster survived deletion = %#v", second)
			}

			term.Apply([]byte(tt.deleteFirst))
			if term.CursorX != start {
				t.Fatalf("cursor after deleting first cluster = %d, want %d", term.CursorX, start)
			}
			if first := term.GridRows[0].Cells[start]; first.Cluster != "" || first.Width != 1 {
				t.Fatalf("first cluster survived deletion = %#v", first)
			}
		})
	}
}

func TestBackspaceRemainsColumnBasedForNaturallyWideClusters(t *testing.T) {
	for _, cluster := range []string{"界", "🧸", "⌚️"} {
		t.Run(cluster, func(t *testing.T) {
			term := newTerminal(6, 1)
			term.Apply([]byte(cluster))
			term.Apply([]byte("\b"))
			if term.CursorX != 1 {
				t.Fatalf("cursor after first BS = %d, want continuation column 1", term.CursorX)
			}
			term.Apply([]byte("\b"))
			if term.CursorX != 0 {
				t.Fatalf("cursor after second BS = %d, want anchor column 0", term.CursorX)
			}
		})
	}
}

func TestCSICursorMovementCanStillAddressBedContinuation(t *testing.T) {
	term := newTerminal(6, 1)
	term.Apply([]byte("🛏️"))
	term.Apply([]byte("\x1b[1D"))
	if term.CursorX != 1 || term.GridRows[0].Cells[term.CursorX].Width != 0 {
		t.Fatalf("CSI CUB cursor=%d cell=%#v, want continuation column", term.CursorX, term.GridRows[0].Cells[term.CursorX])
	}
}

func TestCombiningMarkExtendsTailAcrossSGRWithoutTakingNewStyle(t *testing.T) {
	term := newTerminal(4, 1)
	term.Apply([]byte("e\x1b[31m\u0301X"))
	if got := term.GridRows[0].Cells[0]; got.Cluster != "e\u0301" || got.Width != 1 || got.StyleID != 0 {
		t.Fatalf("combined anchor=%#v", got)
	}
	if got := term.GridRows[0].Cells[1]; got.Cluster != "X" || got.StyleID == 0 {
		t.Fatalf("following styled cell=%#v", got)
	}
}

func TestVS16PromotionAtFinalColumnMatchesAtomicPlacement(t *testing.T) {
	atomic := newTerminal(4, 2)
	atomic.Apply([]byte("abc❤️"))
	fragmented := newTerminal(4, 2)
	fragmented.Apply([]byte("abc❤"))
	fragmented.Apply([]byte("️"))
	if !reflect.DeepEqual(fragmented.GridRows, atomic.GridRows) || fragmented.CursorX != atomic.CursorX || fragmented.CursorY != atomic.CursorY || fragmented.wrapPending != atomic.wrapPending {
		t.Fatalf("fragmented grid=%#v cursor=%d,%d; atomic grid=%#v cursor=%d,%d", fragmented.GridRows, fragmented.CursorX, fragmented.CursorY, atomic.GridRows, atomic.CursorX, atomic.CursorY)
	}
}

func TestVS16PromotionWithoutRoomUsesWidthDegradedAnchor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cols    int
		disable bool
	}{
		{name: "one column", cols: 1},
		{name: "autowrap disabled", cols: 4, disable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := newTerminal(tc.cols, 2)
			if tc.disable {
				term.AutoWrap = false
				term.CursorX = tc.cols - 1
			}
			term.Apply([]byte("❤"))
			term.Apply([]byte("️"))
			anchor := term.GridRows[0].Cells[tc.cols-1]
			if anchor.Cluster != "❤️" || anchor.Width != 1 {
				t.Fatalf("degraded anchor = %#v", anchor)
			}
		})
	}
}

func TestChineseRuneOccupiesTwoCells(t *testing.T) {
	term := newTerminal(5, 1)
	encoded := []byte("界")
	term.Apply(encoded[:1])
	term.Apply(encoded[1:2])
	update := term.Apply(encoded[2:])

	if got := term.GridRows[0].Cells[0]; got.Cluster != "界" || got.Width != 2 {
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
	term := newTerminal(4, 2)
	term.Apply([]byte("abc界"))

	if term.GridRows[1].Cells[0].Cluster != "界" || term.GridRows[1].Cells[0].Width != 2 {
		t.Fatalf("wrapped row = %#v", term.GridRows[1].Cells)
	}
	if term.CursorY != 1 || term.CursorX != 2 {
		t.Fatalf("cursor = %d,%d, want 2,1", term.CursorX, term.CursorY)
	}
}

func TestCROverwrite(t *testing.T) {
	term := newTerminal(5, 1)
	term.Apply([]byte("10%\r30%"))
	got := rowString(term, 0, 3)
	if got != "30%" {
		t.Fatalf("got %q", got)
	}
}

func TestLFScrollAtBottom(t *testing.T) {
	term := newTerminal(3, 2)
	term.Apply([]byte("aaa\nbbb\nccc"))
	row0 := rowString(term, 0, 3)
	row1 := rowString(term, 1, 3)
	if row0 != "bbb" || row1 != "ccc" {
		t.Fatalf("rows = %q / %q", row0, row1)
	}
}

func TestFullPrimaryScrollCapturesBoundedHistory(t *testing.T) {
	term := newTerminal(3, 2)
	term.HistoryLimit = 2
	term.Apply([]byte("aaa\nbbb\nccc\nddd"))
	if len(term.History) != 2 {
		t.Fatalf("history rows = %d, want 2", len(term.History))
	}
	history, _ := term.SnapshotRows()
	if got := cellsString(history[0].Cells); got != "aaa" {
		t.Fatalf("oldest retained history row = %q, want aaa", got)
	}
	if got := cellsString(history[1].Cells); got != "bbb" {
		t.Fatalf("newest retained history row = %q, want bbb", got)
	}
}

func TestSustainedAutowrapReusesTerminalStorage(t *testing.T) {
	term := newTerminal(8, 3)
	term.HistoryLimit = 4
	term.Apply(bytes.Repeat([]byte{'x'}, 8*(term.Rows+term.HistoryLimit+1)))

	rows := make(map[*Cell]struct{}, term.Rows+term.HistoryLimit)
	for _, row := range term.GridRows {
		rows[&row.Cells[0]] = struct{}{}
	}
	for _, row := range term.History {
		rows[&row.Cells[0]] = struct{}{}
	}
	if len(rows) != term.Rows+term.HistoryLimit {
		t.Fatalf("row buffers alias before reuse: got %d, want %d", len(rows), term.Rows+term.HistoryLimit)
	}

	term.Apply(bytes.Repeat([]byte{'y'}, 8*100))
	for _, row := range append(append([]Row(nil), term.GridRows...), term.History...) {
		if _, ok := rows[&row.Cells[0]]; !ok {
			t.Fatal("scroll allocated a new row buffer after history reached its limit")
		}
	}
}

func TestHistoryRingSnapshotPreservesChronologicalOrder(t *testing.T) {
	term := newTerminal(3, 2)
	term.HistoryLimit = 3
	term.Apply([]byte("000\r\n111\r\n222\r\n333\r\n444\r\n555\r\n"))
	history, _ := term.SnapshotRows()
	if len(history) != 3 {
		t.Fatalf("history rows = %d, want 3", len(history))
	}
	for i, want := range []string{"222", "333", "444"} {
		if got := cellsString(history[i].Cells); got != want {
			t.Fatalf("history[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestApplyIntoAccumulatesChunkedDamage(t *testing.T) {
	chunked := newTerminal(4, 2)
	var update Update
	update.Reset(chunked.Rows)
	chunked.ApplyInto([]byte("abcd"), &update)
	chunked.ApplyInto([]byte("efgh"), &update)

	whole := newTerminal(4, 2)
	want := whole.Apply([]byte("abcdefgh"))
	for row := range chunked.GridRows {
		if got, expected := cellsString(chunked.GridRows[row].Cells), cellsString(whole.GridRows[row].Cells); got != expected {
			t.Fatalf("row %d = %q, want %q", row, got, expected)
		}
	}
	if update.ScrollDelta != want.ScrollDelta || update.FullRedraw != want.FullRedraw || update.CursorChanged != want.CursorChanged {
		t.Fatalf("chunked update = %#v, whole update = %#v", update, want)
	}
	for row := range update.DirtySpans {
		if update.DirtySpans[row] != want.DirtySpans[row] {
			t.Fatalf("damage row %d = %#v, want %#v", row, update.DirtySpans[row], want.DirtySpans[row])
		}
	}
}

func TestApplyIntoCanSkipDamageTracking(t *testing.T) {
	term := newTerminal(4, 1)
	var update Update
	update.ResetFor(term.Rows, false)
	term.ApplyInto([]byte("data"), &update)
	if len(update.DirtySpans) != 0 || update.HasDamage() {
		t.Fatalf("detached update retained damage: %#v", update.DirtySpans)
	}
	if got := rowString(term, 0, 4); got != "data" {
		t.Fatalf("detached parsing row = %q", got)
	}
}

func TestOversizedCSIDiscardedAndParserRecovers(t *testing.T) {
	term := newTerminal(4, 1)
	input := append([]byte("\x1b["), bytes.Repeat([]byte{'1'}, maxCSISequenceBytes+100)...)
	input = append(input, 'm', 'O', 'K')
	term.Apply(input)
	if got := rowString(term, 0, 4); got != "OK  " {
		t.Fatalf("row after oversized CSI = %q, want parser to recover", got)
	}
	if len(term.Parser.csiBuf) != 0 || term.Parser.state != parserText {
		t.Fatalf("parser retained oversized CSI: state=%d bytes=%d", term.Parser.state, len(term.Parser.csiBuf))
	}
}

func TestCSIAtBufferLimitConsumesItsFinalByte(t *testing.T) {
	term := newTerminal(4, 1)
	input := append([]byte("\x1b["), bytes.Repeat([]byte{'1'}, maxCSISequenceBytes)...)
	input = append(input, 'm', 'O', 'K')
	term.Apply(input)
	if got := rowString(term, 0, 4); got != "OK  " {
		t.Fatalf("row after CSI at buffer limit = %q", got)
	}
}

func TestOSCContentIsNotRetained(t *testing.T) {
	term := newTerminal(4, 1)
	term.Apply(append([]byte("\x1b]"), bytes.Repeat([]byte{'x'}, 1<<20)...))
	if term.Parser.state != parserOSC {
		t.Fatalf("parser state = %d, want OSC awaiting terminator", term.Parser.state)
	}
	term.Apply([]byte("\aOK"))
	if got := rowString(term, 0, 4); got != "OK  " {
		t.Fatalf("row after OSC terminator = %q", got)
	}
}

func TestTerminalStyleTableIsBounded(t *testing.T) {
	term := newTerminal(1, 1)
	for i := 0; i < maxTerminalStyles+100; i++ {
		term.styleID(Style{FG: protocol.Color{Mode: "rgb", R: uint8(i >> 16), G: uint8(i >> 8), B: uint8(i)}})
	}
	if got := len(term.styleByID); got != maxTerminalStyles {
		t.Fatalf("style table size = %d, want %d", got, maxTerminalStyles)
	}
}

func TestTerminalStyleTablePreallocatesCommonCapacity(t *testing.T) {
	term := newTerminal(1, 1)
	if cap(term.styleByID) < initialStyleCapacity {
		t.Fatalf("style capacity = %d, want at least %d", cap(term.styleByID), initialStyleCapacity)
	}
	first := &term.styleByID[0]
	for i := 1; i < initialStyleCapacity; i++ {
		term.styleID(Style{FG: protocol.Color{Mode: "indexed", Index: uint8(i)}})
	}
	if &term.styleByID[0] != first {
		t.Fatal("ordinary style growth reallocated the preallocated style table")
	}
}

func TestBulkASCIIMatchesBytewiseParsing(t *testing.T) {
	bulk := newTerminal(6, 3)
	bytewise := newTerminal(6, 3)
	prefix := []byte("界界\r\nabcdefghi\x1b[2;2H")
	bulk.Apply(prefix)
	bytewise.Apply(prefix)
	data := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	bulk.Apply(data)
	for _, b := range data {
		bytewise.Apply([]byte{b})
	}
	bulkHistory, bulkPrimary := bulk.SnapshotRows()
	bytewiseHistory, bytewisePrimary := bytewise.SnapshotRows()
	if !reflect.DeepEqual(bulk.GridRows, bytewise.GridRows) || !reflect.DeepEqual(bulkHistory, bytewiseHistory) || !reflect.DeepEqual(bulkPrimary, bytewisePrimary) {
		t.Fatal("bulk ASCII parsing produced different screen or history rows")
	}
	if bulk.CursorX != bytewise.CursorX || bulk.CursorY != bytewise.CursorY || bulk.wrapPending != bytewise.wrapPending || bulk.LastPrintedCluster != bytewise.LastPrintedCluster || bulk.LastPrintedValid != bytewise.LastPrintedValid {
		t.Fatalf("bulk cursor state = (%d,%d,%v,%q,%v), bytewise = (%d,%d,%v,%q,%v)", bulk.CursorX, bulk.CursorY, bulk.wrapPending, bulk.LastPrintedCluster, bulk.LastPrintedValid, bytewise.CursorX, bytewise.CursorY, bytewise.wrapPending, bytewise.LastPrintedCluster, bytewise.LastPrintedValid)
	}
}

func BenchmarkSustainedAutowrap(b *testing.B) {
	term := newTerminal(80, 24)
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	term.Apply(bytes.Repeat([]byte{'w'}, 80*(term.Rows+term.HistoryLimit)))
	var update Update
	update.Reset(term.Rows)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		update.Reset(term.Rows)
		term.ApplyInto(chunk, &update)
	}
}

func BenchmarkDetachedBinaryParsing(b *testing.B) {
	term := newTerminal(80, 24)
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	term.Apply(bytes.Repeat([]byte{'w'}, 80*(term.Rows+term.HistoryLimit)))
	var update Update
	update.ResetFor(term.Rows, false)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		update.ResetFor(term.Rows, false)
		term.ApplyInto(chunk, &update)
	}
}

func BenchmarkDetachedRandomParsing(b *testing.B) {
	term := newTerminal(80, 24)
	chunk := make([]byte, 32<<10)
	state := uint32(0x12345678)
	for i := range chunk {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		chunk[i] = byte(state)
	}
	term.Apply(bytes.Repeat([]byte{'w'}, 80*(term.Rows+term.HistoryLimit)))
	var update Update
	update.ResetFor(term.Rows, false)
	term.ApplyInto(chunk, &update)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		update.ResetFor(term.Rows, false)
		term.ApplyInto(chunk, &update)
	}
}

func BenchmarkBoundedCSIParsing(b *testing.B) {
	term := newTerminal(80, 24)
	chunk := bytes.Repeat([]byte("\x1b[31mX\x1b[0m"), (32<<10)/11)
	var update Update
	update.Reset(term.Rows)
	term.ApplyInto(chunk, &update)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		update.Reset(term.Rows)
		term.ApplyInto(chunk, &update)
	}
}

func TestPartialScrollRegionDoesNotAppendHistory(t *testing.T) {
	term := newTerminal(4, 4)
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
	term := newTerminal(3, 2)
	update := term.Apply([]byte("a\n"))
	if update.FullRedraw {
		t.Fatal("newline without scroll forced full redraw")
	}
}

func TestFullViewportLineFeedReportsScrollAndNewRowDamage(t *testing.T) {
	term := newTerminal(3, 2)
	term.Apply([]byte("aaa\r\nbbb"))

	update := term.Apply([]byte("\r\nccc"))

	if update.FullRedraw {
		t.Fatal("full viewport scroll forced full redraw")
	}
	if update.ScrollDelta != -1 {
		t.Fatalf("scroll delta = %d, want -1", update.ScrollDelta)
	}
	if update.DirtySpans[0].End != 0 || update.DirtySpans[1].End == 0 {
		t.Fatalf("dirty spans = %#v, want only newly exposed row", update.DirtySpans)
	}
	if got, want := update.DirtySpans[1], (DirtySpan{Start: 0, End: 3}); got != want {
		t.Fatalf("bottom row damage = %#v, want %#v", got, want)
	}
}

func TestBareLineFeedReportsStyledExposedRow(t *testing.T) {
	term := newTerminal(3, 2)
	term.Apply([]byte("aaa\r\nbbb\x1b[44m"))

	update := term.Apply([]byte("\n"))

	if update.ScrollDelta != -1 || update.FullRedraw {
		t.Fatalf("bare line-feed update = %#v", update)
	}
	if got, want := update.DirtySpans[1], (DirtySpan{Start: 0, End: 3}); got != want {
		t.Fatalf("exposed-row damage = %#v, want %#v", got, want)
	}
	style := term.styleByID[term.GridRows[1].Cells[0].StyleID]
	if style.BG.Mode != "indexed" || style.BG.Index != 4 {
		t.Fatalf("exposed-row style = %#v", style)
	}
}

func TestLineFeedOutsideScrollRegionStaysWithinGrid(t *testing.T) {
	term := newTerminal(4, 4)
	term.Apply([]byte("\x1b[2;3r\x1b[4;1H\nX"))
	if term.CursorY != 3 || term.GridRows[3].Cells[0].Cluster != "X" {
		t.Fatalf("cursor=%d,%d row=%q", term.CursorX, term.CursorY, rowString(term, 3, 4))
	}
}

func TestPartialRegionLineFeedStillRequiresFullRedraw(t *testing.T) {
	term := newTerminal(4, 4)
	term.Apply([]byte("\x1b[2;3r\x1b[3;1H"))

	update := term.Apply([]byte("\n"))

	if !update.FullRedraw || update.ScrollDelta != 0 {
		t.Fatalf("partial scroll update = %#v, want full redraw without viewport scroll", update)
	}
}

func TestPrintableOutputTracksDirtyColumnSpan(t *testing.T) {
	term := newTerminal(10, 1)
	term.CursorX = 4
	update := term.Apply([]byte("l"))
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 4, End: 5}); got != want {
		t.Fatalf("dirty span = %#v, want %#v", got, want)
	}
}

func TestPrintableOutputMergesDirtyColumnSpans(t *testing.T) {
	term := newTerminal(10, 1)
	update := term.Apply([]byte("abc"))
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 0, End: 3}); got != want {
		t.Fatalf("dirty span = %#v, want %#v", got, want)
	}
}

func TestEraseLine(t *testing.T) {
	term := newTerminal(5, 1)
	term.Apply([]byte("hello"))
	update := term.Apply([]byte("\x1b[3G\x1b[K"))
	got := rowString(term, 0, 5)
	if got != "he   " {
		t.Fatalf("got %q", got)
	}
	if update.FullRedraw {
		t.Fatal("erase line forced a full redraw")
	}
	if update.DirtySpans[0].End == 0 {
		t.Fatal("erase line did not mark its row dirty")
	}
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 2, End: 5}); got != want {
		t.Fatalf("erase line dirty span = %#v, want %#v", got, want)
	}
}

func TestClearErasesEntireCanonicalGrid(t *testing.T) {
	term := newTerminal(8, 3)
	term.Apply([]byte("first\nsecond\nthird"))
	update := term.Apply([]byte("\x1b[H\x1b[2J\x1b[3J"))
	if !update.FullRedraw {
		t.Fatal("clear did not request full redraw")
	}
	for row := range term.GridRows {
		for column, cell := range term.GridRows[row].Cells {
			if cell.Cluster != "" {
				t.Fatalf("cell %d,%d survived clear: %#v", row, column, cell)
			}
		}
	}
}

func TestAlternateScreenRestoresPrimaryGrid(t *testing.T) {
	term := newTerminal(8, 3)
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
	term := newTerminal(8, 3)
	term.Apply([]byte("primary"))
	term.Apply([]byte("\x1b[?1049hTUI"))
	term.Resize(12, 4)
	term.Apply([]byte("\x1b[?1049l"))
	if got := cellsString(term.GridRows[0].Cells[:7]); got != "primary" {
		t.Fatalf("primary after alternate resize=%q", got)
	}
}

func TestApplicationCursorMode(t *testing.T) {
	term := newTerminal(8, 3)
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
	term := newTerminal(5, 2)
	term.Apply([]byte("hello"))
	term.CursorX = 1
	update := term.Apply([]byte("\x1b[2X"))
	if update.FullRedraw {
		t.Fatal("erase characters forced a full redraw")
	}
	if update.DirtySpans[0].End == 0 || update.DirtySpans[1].End != 0 {
		t.Fatal("erase characters did not mark its row dirty")
	}
	if got, want := update.DirtySpans[0], (DirtySpan{Start: 1, End: 3}); got != want {
		t.Fatalf("erase characters dirty span = %#v, want %#v", got, want)
	}
}

func TestInsertCharactersShiftsExistingTextRight(t *testing.T) {
	term := newTerminal(12, 1)
	term.Apply([]byte("abcdef\r\x1b[3C\x1b[3@zzz"))
	if got := cellsString(term.GridRows[0].Cells[:9]); got != "abczzzdef" {
		t.Fatalf("inserted text=%q", got)
	}
}

func TestRepeatPrecedingCharacterPreservesStyle(t *testing.T) {
	term := newTerminal(8, 1)
	update := term.Apply([]byte("\x1b[44m \x1b[6b"))
	if update.FullRedraw {
		t.Fatal("REP forced full redraw")
	}
	for column := 0; column < 7; column++ {
		cell := term.GridRows[0].Cells[column]
		if cell.Cluster != "" || term.styleByID[cell.StyleID].BG.Index != 4 {
			t.Fatalf("column %d=%#v style=%#v", column, cell, term.styleByID[cell.StyleID])
		}
	}
}

func TestRepeatPrecedingCharacterRepeatsCompleteCluster(t *testing.T) {
	for _, cluster := range []string{"e\u0301", "👩‍💻"} {
		t.Run(cluster, func(t *testing.T) {
			term := newTerminal(12, 1)
			term.Apply([]byte(cluster + "\x1b[2b"))
			width := int(clusterCellWidth(cluster))
			for column := 0; column < width*3; column += width {
				if cell := term.GridRows[0].Cells[column]; cell.Cluster != cluster || int(cell.Width) != width {
					t.Fatalf("repeated cluster at column %d = %#v", column, cell)
				}
			}
		})
	}
}

func TestDeleteCharactersUsesCurrentBackground(t *testing.T) {
	term := newTerminal(8, 1)
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
	term := newTerminal(2, 1)
	term.Apply([]byte("\x1b[1;38;2;1;2;3;7mA"))
	cell := term.GridRows[0].Cells[0]
	style := term.styleByID[cell.StyleID]
	if !style.Bold || !style.Reverse || style.FG.Mode != "rgb" || style.FG.R != 1 {
		t.Fatalf("style = %#v", style)
	}
}

func TestOSCIsConsumedAndNotPrinted(t *testing.T) {
	term := newTerminal(32, 1)
	term.Apply([]byte("\x1b]0;garindra@garindra-ubuntu: ~\x07garindra@garindra-ubuntu:~$ "))
	got := rowString(term, 0, 28)
	want := "garindra@garindra-ubuntu:~$ "
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOSCSTTerminatorIsConsumed(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("\x1b]0;title\x1b\\prompt"))
	got := rowString(term, 0, 6)
	if got != "prompt" {
		t.Fatalf("got %q", got)
	}
}

func TestDCSAndUTF8DesignationAreConsumed(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("\x1b%G\x1bP$qm\x1b\\prompt"))
	if got := rowString(term, 0, 6); got != "prompt" {
		t.Fatalf("control strings leaked into display: %q", got)
	}
}

func TestCANAndSUBCancelIncompleteControlSequences(t *testing.T) {
	term := newTerminal(4, 1)
	term.Apply([]byte("\x1b[31\x18\x1b[1\x1aOK"))
	if got := rowString(term, 0, 2); got != "OK" {
		t.Fatalf("canceled controls leaked into display: %q", got)
	}
}

func TestCharsetDesignationIsConsumedAndNotPrinted(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("\x1b(Bprompt"))
	got := rowString(term, 0, 6)
	if got != "prompt" {
		t.Fatalf("got %q", got)
	}
}

func TestResizePreservesVisibleContent(t *testing.T) {
	term := newTerminal(5, 2)
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
	term := newTerminal(5, 2)
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

func TestResizeNeverSplitsClusterAcrossRows(t *testing.T) {
	for _, cluster := range []string{"क्ष", "葛\U000e0100", "👩‍💻"} {
		t.Run(cluster, func(t *testing.T) {
			term := newTerminal(5, 2)
			term.Apply([]byte("abc" + cluster))

			term.Resize(4, 3)
			if got := rowString(term, 0, 4); got != "abc " {
				t.Fatalf("row 0 after cluster-aware reflow = %q", got)
			}
			if anchor, continuation := term.GridRows[1].Cells[0], term.GridRows[1].Cells[1]; anchor.Cluster != cluster || anchor.Width != 2 || continuation.Width != 0 {
				t.Fatalf("row 1 cluster after reflow = %#v", term.GridRows[1].Cells[:2])
			}
			if !term.GridRows[0].WrapsNext {
				t.Fatal("cluster-aware reflow did not preserve the soft-wrap chain")
			}
		})
	}
}

func TestResizeShrinkKeepsBottomContentVisible(t *testing.T) {
	term := newTerminal(5, 2)
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
	term := newTerminal(5, 2)
	term.Apply([]byte("abcdef"))
	if !term.GridRows[0].WrapsNext {
		t.Fatal("row 0 should wrap into row 1")
	}
	if term.GridRows[1].WrapsNext {
		t.Fatal("row 1 should not wrap onward")
	}
}

func TestLFCreatesHardBoundary(t *testing.T) {
	term := newTerminal(5, 2)
	term.Apply([]byte("abc\n"))
	if term.GridRows[0].WrapsNext {
		t.Fatal("newline should not leave soft-wrap metadata behind")
	}
}

func TestCRDoesNotCreateFalseWrapChain(t *testing.T) {
	term := newTerminal(5, 2)
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
	term := newTerminal(8, 2)
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
	setTerminalDebugLogger(&buf)
	defer setTerminalDebugLogger(nil)

	term := newTerminal(5, 1)
	term.Apply([]byte("\x1b[1v"))

	if got := buf.String(); !strings.Contains(got, "unsupported CSI 1v") {
		t.Fatalf("debug log = %q", got)
	}
}

func TestSaveRestoreCursorAndStyle(t *testing.T) {
	term := newTerminal(5, 2)
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
	term := newTerminal(5, 1)
	term.Apply([]byte("abcde"))
	term.CursorX = 1
	term.Apply([]byte("\x1b[1P"))
	if got := rowString(term, 0, 5); got != "acde " {
		t.Fatalf("row after DCH = %q", got)
	}
}

func TestScrollRegionLineFeedScrollsWithinMargins(t *testing.T) {
	term := newTerminal(4, 4)
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
	term := newTerminal(4, 4)
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
	term := newTerminal(5, 2)
	term.CursorX = 2
	term.CursorY = 1
	update := term.Apply([]byte("\x1b[6n"))
	if len(update.Replies) != 1 || string(update.Replies[0]) != "\x1b[2;3R" {
		t.Fatalf("DSR replies = %#v", update.Replies)
	}
}

func TestDECSpecialGraphicsG0AndFragmentedDesignation(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("\x1b("))
	term.Apply([]byte("0lqkxmqj"))
	term.Apply([]byte("\x1b(Bq"))
	if got := rowString(term, 0, 8); got != "┌─┐│└─┘q" {
		t.Fatalf("DEC graphics row = %q", got)
	}
}

func TestDECSpecialGraphicsG1SelectedByShiftOut(t *testing.T) {
	term := newTerminal(8, 1)
	term.Apply([]byte("\x1b)0\x0elqk\x0fq"))
	if got := rowString(term, 0, 4); got != "┌─┐q" {
		t.Fatalf("G1 graphics row = %q", got)
	}
}

func TestCursorSaveRestoreIncludesCharset(t *testing.T) {
	term := newTerminal(4, 1)
	term.Apply([]byte("\x1b(0\x1b7\x1b(B\x1b8q"))
	if got := term.GridRows[0].Cells[0].Cluster; got != "─" {
		t.Fatalf("restored charset rendered %q", got)
	}
}

func TestIndexNextLineAndReverseIndex(t *testing.T) {
	term := newTerminal(4, 3)
	term.Apply([]byte("a\x1bDb\x1bEc"))
	if term.CursorX != 1 || term.CursorY != 2 || term.GridRows[1].Cells[1].Cluster != "b" || term.GridRows[2].Cells[0].Cluster != "c" {
		t.Fatalf("IND/NEL state cursor=%d,%d rows=%q/%q", term.CursorX, term.CursorY, rowString(term, 1, 4), rowString(term, 2, 4))
	}
}

func TestVerticalTabAndFormFeedActAsLineFeed(t *testing.T) {
	term := newTerminal(4, 3)
	term.Apply([]byte("a\vb\fc"))
	if term.CursorY != 2 || term.GridRows[1].Cells[1].Cluster != "b" || term.GridRows[2].Cells[2].Cluster != "c" {
		t.Fatalf("VT/FF cursor=%d,%d rows=%q/%q", term.CursorX, term.CursorY, rowString(term, 1, 4), rowString(term, 2, 4))
	}
}

func TestTabStopsHTSTBCCBTAndResize(t *testing.T) {
	term := newTerminal(20, 1)
	term.Apply([]byte("\t"))
	if term.CursorX != 8 {
		t.Fatalf("default tab cursor=%d", term.CursorX)
	}
	term.Apply([]byte("\x1b[g\r\t"))
	if term.CursorX != 16 {
		t.Fatalf("cleared first stop cursor=%d", term.CursorX)
	}
	term.CursorX = 5
	term.Apply([]byte("\x1bH\r\t\x1b[Z"))
	if term.CursorX != 0 {
		t.Fatalf("HTS/CBT cursor=%d", term.CursorX)
	}
	term.Resize(28, 1)
	term.CursorX = 17
	term.Apply([]byte("\t"))
	if term.CursorX != 24 {
		t.Fatalf("grown default tab cursor=%d", term.CursorX)
	}
	term.Apply([]byte("\x1b[3g\r\t"))
	if term.CursorX != 27 {
		t.Fatalf("clear-all tabs cursor=%d", term.CursorX)
	}
}

func TestInsertDeleteLinesWithinScrollRegion(t *testing.T) {
	term := newTerminal(4, 4)
	term.Apply([]byte("1111\r\n2222\r\n3333\r\n4444"))
	term.Apply([]byte("\x1b[2;3r\x1b[2;1H\x1b[L"))
	if got := []string{rowString(term, 0, 4), rowString(term, 1, 4), rowString(term, 2, 4), rowString(term, 3, 4)}; strings.Join(got, "/") != "1111/    /2222/4444" {
		t.Fatalf("IL rows=%v", got)
	}
	term.Apply([]byte("\x1b[M"))
	if got := []string{rowString(term, 0, 4), rowString(term, 1, 4), rowString(term, 2, 4), rowString(term, 3, 4)}; strings.Join(got, "/") != "1111/2222/    /4444" {
		t.Fatalf("DL rows=%v", got)
	}
}

func TestScrollUpDownCommandsAndDamage(t *testing.T) {
	term := newTerminal(4, 3)
	term.Apply([]byte("1111\r\n2222\r\n3333"))
	update := term.Apply([]byte("\x1b[S"))
	if update.ScrollDelta != -1 || update.FullRedraw || rowString(term, 0, 4) != "2222" {
		t.Fatalf("SU update=%#v row0=%q", update, rowString(term, 0, 4))
	}
	update = term.Apply([]byte("\x1b[T"))
	if update.ScrollDelta != 1 || update.FullRedraw || rowString(term, 0, 4) != "    " {
		t.Fatalf("SD update=%#v row0=%q", update, rowString(term, 0, 4))
	}
}

func TestScrollCommandsUseCurrentBackgroundForExposedRows(t *testing.T) {
	term := newTerminal(3, 2)
	term.Apply([]byte("abc\r\ndef\x1b[44m\x1b[S"))
	style := term.styleByID[term.GridRows[1].Cells[0].StyleID]
	if style.BG.Mode != "indexed" || style.BG.Index != 4 {
		t.Fatalf("scrolled blank style=%#v", style)
	}
}

func TestOriginModePositionsRelativeToMargins(t *testing.T) {
	term := newTerminal(8, 6)
	term.Apply([]byte("\x1b[2;5r\x1b[?6h\x1b[2;3H"))
	if term.CursorX != 2 || term.CursorY != 2 {
		t.Fatalf("origin CUP cursor=%d,%d", term.CursorX, term.CursorY)
	}
	term.Apply([]byte("\x1b[99B"))
	if term.CursorY != 4 {
		t.Fatalf("origin movement escaped bottom margin: %d", term.CursorY)
	}
	term.Apply([]byte("\x1b[?6l"))
	if term.CursorX != 0 || term.CursorY != 0 {
		t.Fatalf("origin reset did not home cursor: %d,%d", term.CursorX, term.CursorY)
	}
}

func TestAutowrapAndInsertModes(t *testing.T) {
	term := newTerminal(5, 2)
	term.Apply([]byte("\x1b[?7labcdef"))
	if term.CursorY != 0 || rowString(term, 0, 5) != "abcdf" {
		t.Fatalf("disabled autowrap cursor=%d,%d row=%q", term.CursorX, term.CursorY, rowString(term, 0, 5))
	}
	term.Apply([]byte("\r\x1b[4hZ"))
	if got := rowString(term, 0, 5); got != "Zabcd" {
		t.Fatalf("insert mode row=%q", got)
	}
}

func TestInsertModePreservesWideCellIntegrity(t *testing.T) {
	term := newTerminal(6, 1)
	term.Apply([]byte("界ab\r\x1b[4hZ"))
	if got := rowString(term, 0, 5); got != "Z界 ab" {
		t.Fatalf("wide insert row=%q", got)
	}
	if term.GridRows[0].Cells[1].Width != 2 || term.GridRows[0].Cells[2].Width != 0 {
		t.Fatalf("wide insert cells=%#v", term.GridRows[0].Cells)
	}
}

func TestCSIAndDECCursorSaveRestore(t *testing.T) {
	for _, sequences := range [][2]string{{"\x1b[s", "\x1b[u"}, {"\x1b7", "\x1b8"}} {
		term := newTerminal(5, 2)
		term.CursorX, term.CursorY = 3, 1
		term.Apply([]byte(sequences[0]))
		term.CursorX, term.CursorY = 0, 0
		term.Apply([]byte(sequences[1]))
		if term.CursorX != 3 || term.CursorY != 1 {
			t.Fatalf("save/restore %q cursor=%d,%d", sequences, term.CursorX, term.CursorY)
		}
	}
}

func TestParameterizedCSISIsNotMistakenForCursorSave(t *testing.T) {
	term := newTerminal(8, 2)
	term.CursorX, term.CursorY = 3, 1
	term.Apply([]byte("\x1b[1;7s")) // DECSLRM when horizontal-margin mode is enabled.
	term.CursorX, term.CursorY = 0, 0
	term.Apply([]byte("\x1b[u"))
	if term.CursorX != 0 || term.CursorY != 0 {
		t.Fatalf("unsupported DECSLRM was mistaken for cursor save: %d,%d", term.CursorX, term.CursorY)
	}
}

func TestDEC1048And1049CursorAndCharsetSemantics(t *testing.T) {
	term := newTerminal(8, 2)
	term.CursorX, term.CursorY = 3, 1
	term.Apply([]byte("\x1b[?1048h\x1b[H\x1b[?1048l"))
	if term.CursorX != 3 || term.CursorY != 1 {
		t.Fatalf("1048 cursor=%d,%d", term.CursorX, term.CursorY)
	}

	term.Apply([]byte("\x1b(0"))
	term.CursorX, term.CursorY = 4, 1
	term.Apply([]byte("\x1b[?1049h\x1b(Balt\x1b[?1049lq"))
	if term.Alternate || term.CursorY != 1 || term.GridRows[1].Cells[4].Cluster != "─" {
		t.Fatalf("1049 restore alternate=%v cursor=%d,%d row=%q charset=%q", term.Alternate, term.CursorX, term.CursorY, rowString(term, 1, 8), term.G0Charset)
	}
}

func TestDECCursorSaveRestoreIncludesOriginMode(t *testing.T) {
	term := newTerminal(8, 6)
	term.Apply([]byte("\x1b[2;5r\x1b[?6h\x1b[3;2H\x1b7\x1b[?6l\x1b8"))
	if !term.OriginMode || term.CursorX != 1 || term.CursorY != 3 {
		t.Fatalf("restored origin=%v cursor=%d,%d", term.OriginMode, term.CursorX, term.CursorY)
	}
}

func TestSGRDimBlinkInvisibleAndResets(t *testing.T) {
	term := newTerminal(4, 1)
	term.Apply([]byte("\x1b[1;2;5;8mA\x1b[22;25;28mB"))
	first := term.styleByID[term.GridRows[0].Cells[0].StyleID]
	second := term.styleByID[term.GridRows[0].Cells[1].StyleID]
	if !first.Bold || !first.Dim || !first.Blink || !first.Invisible {
		t.Fatalf("set style=%#v", first)
	}
	if second.Bold || second.Dim || second.Blink || second.Invisible {
		t.Fatalf("reset style=%#v", second)
	}
}

func TestED3ClearsOnlyHistory(t *testing.T) {
	term := newTerminal(4, 2)
	term.Apply([]byte("1111\r\n2222\r\n3333"))
	before := rowString(term, 0, 4) + rowString(term, 1, 4)
	update := term.Apply([]byte("\x1b[3J"))
	if len(term.History) != 0 || rowString(term, 0, 4)+rowString(term, 1, 4) != before || update.FullRedraw {
		t.Fatalf("ED3 history=%d screen=%q update=%#v", len(term.History), rowString(term, 0, 4)+rowString(term, 1, 4), update)
	}
}

func TestSoftAndHardReset(t *testing.T) {
	term := newTerminal(4, 2)
	term.HistoryLimit = 17
	term.Apply([]byte("text\x1b[?6h\x1b[4h\x1b[!p"))
	if term.OriginMode || term.InsertMode || !term.AutoWrap || rowString(term, 0, 4) != "text" {
		t.Fatalf("soft reset state origin=%v insert=%v wrap=%v row=%q", term.OriginMode, term.InsertMode, term.AutoWrap, rowString(term, 0, 4))
	}
	term.Apply([]byte("\x1bc"))
	if rowString(term, 0, 4) != "    " || term.CursorX != 0 || term.CursorY != 0 || term.HistoryLimit != 17 {
		t.Fatalf("hard reset row=%q cursor=%d,%d history-limit=%d", rowString(term, 0, 4), term.CursorX, term.CursorY, term.HistoryLimit)
	}
}

func rowString(term *TerminalState, row, count int) string {
	var text strings.Builder
	for i := 0; i < count; i++ {
		cluster := term.GridRows[row].Cells[i].Cluster
		if cluster == "" {
			text.WriteByte(' ')
		} else {
			text.WriteString(cluster)
		}
	}
	return text.String()
}

func cellsString(cells []Cell) string {
	var text strings.Builder
	for _, cell := range cells {
		if cell.Cluster == "" {
			text.WriteByte(' ')
		} else {
			text.WriteString(cell.Cluster)
		}
	}
	return text.String()
}
