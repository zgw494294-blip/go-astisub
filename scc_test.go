package astisub_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asticode/go-astisub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sccFrameTime mirrors the 29.97 frame duration used by the parser:
// 1001/30000 seconds per frame.
func sccFrameTime(f int64) time.Duration {
	return time.Duration((f*100100000 + 1) / 3)
}

// sccParityByte returns b with a correct odd parity high bit.
func sccParityByte(b byte) byte {
	if bits.OnesCount8(b)%2 == 1 {
		return b
	}
	return b | 0x80
}

// sccWord formats one parity-correct SCC hexadecimal word.
func sccWord(hi, lo byte) string {
	return fmt.Sprintf("%02X%02X", sccParityByte(hi), sccParityByte(lo))
}

var sccPACHilo = map[int][2]int{
	1: {0x11, 0}, 2: {0x11, 1},
	3: {0x12, 0}, 4: {0x12, 1},
	5: {0x15, 0}, 6: {0x15, 1},
	7: {0x16, 0}, 8: {0x16, 1},
	9: {0x17, 0}, 10: {0x17, 1},
	11: {0x10, 0},
	12: {0x13, 0}, 13: {0x13, 1},
	14: {0x14, 0}, 15: {0x14, 1},
}

// sccPAC builds a preamble address code word for the given 1-based row and
// column indent (0, 4, 8, ... 28).
func sccPAC(row, indent int) string {
	x := sccPACHilo[row]
	lo := 0x50 | x[1]<<5 | indent/2
	return sccWord(byte(x[0]), byte(lo))
}

var sccBasicEncode = map[rune]byte{
	'’': 0x27, 'á': 0x2a, 'é': 0x5c, 'í': 0x5e, 'ó': 0x5f,
	'ú': 0x60, 'ç': 0x7b, '÷': 0x7c, 'Ñ': 0x7d, 'ñ': 0x7e, '█': 0x7f,
}

// sccText encodes a basic North American string into hexadecimal words.
func sccText(s string) []string {
	var codes []byte
	for _, r := range s {
		if c, ok := sccBasicEncode[r]; ok {
			codes = append(codes, c)
		} else if r < 0x80 {
			codes = append(codes, byte(r))
		}
	}
	var out []string
	for i := 0; i < len(codes); i += 2 {
		var lo byte
		if i+1 < len(codes) {
			lo = codes[i+1]
		}
		out = append(out, sccWord(codes[i], lo))
	}
	return out
}

// sccRecord builds one SCC record.
func sccRecord(tc string, words ...string) string {
	return tc + "\t" + strings.Join(words, " ")
}

// sccStream assembles a complete SCC stream using LF separators.
func sccStream(records ...string) string {
	return "Scenarist_SCC V1.0\n\n" + strings.Join(records, "\n") + "\n"
}

// sccItemText joins all lines of an item with newlines.
func sccItemText(i *astisub.Item) string {
	var ls []string
	for _, l := range i.Lines {
		ls = append(ls, l.String())
	}
	return strings.Join(ls, "\n")
}

func TestReadFromSCCPopOnTiming(t *testing.T) {
	// First word happens on the labeled frame; every further word advances
	// one frame. The caption appears at EOC and closes one frame after the
	// last transmitted word at EOF.
	stream := sccStream(
		sccRecord("00:00:01:00",
			"9420", "9420", // RCL (retransmitted, acts once)
			sccPAC(1, 0), sccPAC(1, 0),
			"C845", "4C4C", "4F80", // H E, L L, O + pad
			"94AF", "94AF", // EOC (retransmitted, acts once)
		),
	)
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HELLO", s.Items[0].String())
	assert.Equal(t, sccFrameTime(37), s.Items[0].StartAt) // base frame 30 + word 7
	assert.Equal(t, sccFrameTime(39), s.Items[0].EndAt)   // last word frame 38, +1
}

func TestReadFromSCCEDMClosesAtWordFrame(t *testing.T) {
	stream := sccStream(
		sccRecord("00:00:00:00", "9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			sccWord('H', 0x80), "94AF", "94AF"),
		sccRecord("00:00:01:00", "942C", "942C"), // EDM pair at frame 30
	)
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "H", s.Items[0].String())
	assert.Equal(t, sccFrameTime(5), s.Items[0].StartAt)
	assert.Equal(t, sccFrameTime(30), s.Items[0].EndAt)
}

func TestReadFromSCCEmptyOutputs(t *testing.T) {
	// Only a valid header.
	s, err := astisub.ReadFromSCC(strings.NewReader("Scenarist_SCC V1.0\n\n"))
	require.NoError(t, err)
	assert.True(t, s.IsEmpty())

	// Only hidden pop-on loading: EOF never displays hidden content.
	stream := sccStream(
		sccRecord("00:00:00:00", "9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			"C845", "4F80"),
	)
	s, err = astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	assert.True(t, s.IsEmpty())

	// Record without any word is legal.
	s, err = astisub.ReadFromSCC(strings.NewReader("Scenarist_SCC V1.0\n\n00:00:00:00\t\n"))
	require.NoError(t, err)
	assert.True(t, s.IsEmpty())
}

func TestReadFromSCCInterleavedCC2Ignored(t *testing.T) {
	stream := sccStream(
		sccRecord("00:00:00:00",
			sccWord(0x15, 0x20), sccWord(0x15, 0x20), // CC2 RCL
			sccWord(0x00, 'N'), sccWord(0x00, 'X'), // CC2 text
			"9420", "9420", // CC1 RCL
			sccPAC(1, 0), sccPAC(1, 0),
			sccWord('H', 'I'),
			"94AF", "94AF",
		),
	)
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HI", s.Items[0].String())
	// CC1 RCL is word 4, PAC 6, text 8, EOC first at frame 9.
	assert.Equal(t, sccFrameTime(9), s.Items[0].StartAt)
	assert.Equal(t, sccFrameTime(11), s.Items[0].EndAt)
}

func TestReadFromSCCRepeatedTextKeptAndPadsCountFrames(t *testing.T) {
	stream := sccStream(
		sccRecord("00:00:00:00",
			"9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			"4141", "4141", // duplicated TEXT: both apply -> "AAAA"
			"8080", "8080", // padding still occupies transmission frames
			"94AF", "94AF",
		),
	)
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "AAAA", s.Items[0].String())
	assert.Equal(t, sccFrameTime(8), s.Items[0].StartAt)
	assert.Equal(t, sccFrameTime(10), s.Items[0].EndAt)
}

func TestReadFromSCCRollUp3(t *testing.T) {
	words := []string{"9426", "9426", sccPAC(15, 0), sccPAC(15, 0)}
	words = append(words, sccText("L1")...)
	words = append(words, "942D", "942D")
	words = append(words, sccText("L2")...)
	words = append(words, "942D", "942D")
	words = append(words, sccText("L3")...)
	words = append(words, "942D", "942D")
	words = append(words, sccText("L4")...)
	stream := sccStream(sccRecord("00:00:00:00", words...))

	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 5)
	assert.Equal(t, "L1", sccItemText(s.Items[0]))
	assert.Equal(t, sccFrameTime(4), s.Items[0].StartAt)
	assert.Equal(t, sccFrameTime(7), s.Items[0].EndAt)
	assert.Equal(t, "L1\nL2", sccItemText(s.Items[1]))
	assert.Equal(t, sccFrameTime(7), s.Items[1].StartAt)
	assert.Equal(t, sccFrameTime(10), s.Items[1].EndAt)
	assert.Equal(t, "L1\nL2\nL3", sccItemText(s.Items[2]))
	assert.Equal(t, sccFrameTime(10), s.Items[2].StartAt)
	assert.Equal(t, sccFrameTime(11), s.Items[2].EndAt)
	assert.Equal(t, "L2\nL3", sccItemText(s.Items[3]))
	assert.Equal(t, sccFrameTime(11), s.Items[3].StartAt)
	assert.Equal(t, sccFrameTime(13), s.Items[3].EndAt)
	assert.Equal(t, "L2\nL3\nL4", sccItemText(s.Items[4]))
	assert.Equal(t, sccFrameTime(13), s.Items[4].StartAt)
	assert.Equal(t, sccFrameTime(14), s.Items[4].EndAt)
}

func TestReadFromSCCRollUpSizesAndBaseMove(t *testing.T) {
	// RU2 keeps two rows: typing A CR B CR C leaves B and C visible.
	words := []string{"9425", "9425", sccPAC(15, 0), sccPAC(15, 0)}
	for _, w := range [][2]string{{"A", "942D"}, {"B", "942D"}, {"C", "942D"}} {
		words = append(words, sccText(w[0])...)
		words = append(words, w[1], w[1])
	}
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 5)
	assert.Equal(t, "A", sccItemText(s.Items[0]))
	assert.Equal(t, "A\nB", sccItemText(s.Items[1]))
	assert.Equal(t, "B", sccItemText(s.Items[2]))
	assert.Equal(t, "B\nC", sccItemText(s.Items[3]))
	assert.Equal(t, "C", sccItemText(s.Items[4]))

	// Moving the base row with a PAC changes the roll-up window: the old
	// content scrolls out when a CR happens at the new base row.
	words = []string{"9426", "9426", sccPAC(15, 0), sccPAC(15, 0)}
	words = append(words, sccText("A")...)
	words = append(words, "942D", "942D")
	words = append(words, sccPAC(4, 0), sccPAC(4, 0))
	words = append(words, sccText("B")...)
	words = append(words, "942D", "942D")
	s, err = astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	last := s.Items[len(s.Items)-1]
	assert.Equal(t, "B", sccItemText(last))
}

func TestReadFromSCCPaintOn(t *testing.T) {
	words := []string{"9429", "9429", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccText("HE")...)
	words = append(words, sccText("LL")...)
	words = append(words, sccWord('O', 0x00))
	words = append(words, "942C", "942C")
	stream := sccStream(sccRecord("00:00:00:00", words...))

	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 3)
	assert.Equal(t, "HE", s.Items[0].String())
	assert.Equal(t, sccFrameTime(4), s.Items[0].StartAt)
	assert.Equal(t, sccFrameTime(5), s.Items[0].EndAt)
	assert.Equal(t, "HELL", s.Items[1].String())
	assert.Equal(t, "HELLO", s.Items[2].String())
	assert.Equal(t, sccFrameTime(6), s.Items[2].StartAt)
	assert.Equal(t, sccFrameTime(7), s.Items[2].EndAt)
}

func TestReadFromSCCBackspaceAndDER(t *testing.T) {
	// "ABX" then backspace then "C" -> "ABC".
	words := []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccText("ABX")...)
	words = append(words, "9421", "9421", sccWord('C', 0x00))
	words = append(words, "94AF", "94AF")
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "ABC", s.Items[0].String())

	// "ABCDEF", PAC indent 4, DER -> "ABCD".
	words = []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccText("ABCDEF")...)
	words = append(words, sccPAC(1, 4), sccPAC(1, 4), "9424", "9424", "94AF", "94AF")
	s, err = astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "ABCD", s.Items[0].String())
}

func TestReadFromSCCIndentTabAndMidrowSpacing(t *testing.T) {
	// PAC repositioning keeps skipped existing text: "AB" + PAC indent 4
	// + "CD" -> "AB  CD".
	words := []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccText("AB")...)
	words = append(words, sccPAC(1, 4), sccPAC(1, 4), sccWord('C', 'D'))
	words = append(words, "94AF", "94AF")
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "AB  CD", s.Items[0].String())

	// Tab offset 2 moves the cursor without erasing anything.
	words = []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccWord('A', 'B'), sccWord(0x17, 0x22), sccWord(0x17, 0x22))
	words = append(words, sccWord('C', 'D'), "94AF", "94AF")
	s, err = astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "AB  CD", s.Items[0].String())

	// A mid-row code occupies one space cell.
	words = []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccWord('A', 'B'), sccWord(0x11, 0x20), sccWord(0x11, 0x20))
	words = append(words, sccWord('C', 'D'), "94AF", "94AF")
	s, err = astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "AB CD", s.Items[0].String())
}

func TestReadFromSCCCharacterSets(t *testing.T) {
	// Special NA music note, basic overrides é and ’, extended ï replacing
	// the fallback "e" in "naive".
	words := []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words,
		sccWord(0x11, 0x37), // ♪
		sccWord(0x5c, 0x00), // é
		sccWord(0x27, 0x00), // ’
	)
	words = append(words, sccText("naiv")...)
	words = append(words, sccWord('e', 0x00), sccWord(0x12, 0x39)) // e -> ï
	words = append(words, "94AF", "94AF")
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "♪é’naivï", s.Items[0].String())
}

func TestReadFromSCCEOCTwoMemorySwap(t *testing.T) {
	build := func(withENM bool) string {
		var words []string
		// Caption 1: AAA on row 1.
		words = append(words, "9420", "9420", sccPAC(1, 0), sccPAC(1, 0))
		words = append(words, sccWord('A', 'A'), sccWord('A', 0x00), "94AF", "94AF")
		// Caption 2: BBB on row 1 (AAA lands in the hidden memory).
		words = append(words, "9420", "9420", sccPAC(1, 0), sccPAC(1, 0))
		words = append(words, sccWord('B', 'B'), sccWord('B', 0x00), "94AF", "94AF")
		// Caption 3: CCC on row 3. Without ENM, AAA kept in the hidden
		// memory swaps back in.
		words = append(words, "9420", "9420")
		if withENM {
			words = append(words, "942E", "942E")
		}
		words = append(words, sccPAC(3, 0), sccPAC(3, 0))
		words = append(words, sccWord('C', 'C'), sccWord('C', 0x00), "94AF", "94AF")
		return sccStream(sccRecord("00:00:00:00", words...))
	}

	s, err := astisub.ReadFromSCC(strings.NewReader(build(false)))
	require.NoError(t, err)
	require.Len(t, s.Items, 3)
	assert.Equal(t, "AAA", sccItemText(s.Items[0]))
	assert.Equal(t, "BBB", sccItemText(s.Items[1]))
	assert.Equal(t, "AAA\nCCC", sccItemText(s.Items[2]))

	s, err = astisub.ReadFromSCC(strings.NewReader(build(true)))
	require.NoError(t, err)
	require.Len(t, s.Items, 3)
	assert.Equal(t, "CCC", sccItemText(s.Items[2]))
}

func TestReadFromSCCModeSwitchKeepsHiddenLoading(t *testing.T) {
	// Pop-on hidden loading must not show, then switching to roll-up paints
	// only the roll-up content.
	words := []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0)}
	words = append(words, sccWord('H', 'I'), sccWord('D', 0))
	words = append(words, "9426", "9426", sccPAC(15, 0), sccPAC(15, 0))
	words = append(words, sccText("L1")...)
	words = append(words, "942D", "942D")
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:00:00:00", words...))))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "L1", s.Items[0].String())
}

func TestReadFromSCCDropFrameTimecode(t *testing.T) {
	words := []string{"9420", "9420", sccPAC(1, 0), sccPAC(1, 0), sccWord('H', 0x00), "94AF", "94AF"}
	stream := sccStream(sccRecord("00:01:00;02", words...)) // absolute frame 1800
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, sccFrameTime(1800+5), s.Items[0].StartAt)

	// Invalid drop-frame value (frames 0 and 1 do not exist at minute 1).
	_, err = astisub.ReadFromSCC(strings.NewReader(sccStream(sccRecord("00:01:00;00", "8080"))))
	assert.Error(t, err)
}

func TestReadFromSCCCRLF(t *testing.T) {
	stream := strings.ReplaceAll(sccStream(
		sccRecord("00:00:01:00", "9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			sccWord('H', 'I'), "94AF", "94AF"),
	), "\n", "\r\n")
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HI", s.Items[0].String())
}

func TestReadFromSCCWordOnlyLinesAndBOM(t *testing.T) {
	// Word-only lines without a timecode are not records and are ignored.
	stream := "\uFEFFScenarist_SCC V1.0\n\n" +
		"00:00:01:00\t9420 9420 " + sccPAC(1, 0) + " " + sccPAC(1, 0) + " " +
		sccWord('H', 'I') + " 94AF 94AF\n" +
		"913d 913e 91bf\n" // orphan continuation words
	s, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HI", s.Items[0].String())

	// Blank lines (including records with no words) do not reset timing.
	s, err = astisub.ReadFromSCC(strings.NewReader("Scenarist_SCC V1.0\n\n\n00:00:00:00\t\n\n"))
	require.NoError(t, err)
	assert.True(t, s.IsEmpty())
}

func TestReadFromSCCErrors(t *testing.T) {
	for _, stream := range []string{
		"",               // empty
		"not a header\n", // bad header
		"Scenarist_SCC V1.0\n\n00:00:00-00\t8080\n",  // bad timecode
		"Scenarist_SCC V1.0\n\n00:00:00:60\t8080\n",  // frame out of range
		"Scenarist_SCC V1.0\n\n00:00:00:00\t942g\n",  // bad hex word
		"Scenarist_SCC V1.0\n\n00:00:00:00\t942\n",   // word too short
		"Scenarist_SCC V1.0\n\n00:00:00:00\t94200\n", // word too long
	} {
		_, err := astisub.ReadFromSCC(strings.NewReader(stream))
		assert.Error(t, err, stream)
	}

	// Reader failures are propagated.
	_, err := astisub.ReadFromSCC(errReader{})
	assert.Error(t, err)
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("read failure") }

func TestReadFromSCCIndependentReads(t *testing.T) {
	stream := sccStream(
		sccRecord("00:00:01:00", "9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			sccWord('H', 'I'), "94AF", "94AF"),
	)
	s1, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	s2, err := astisub.ReadFromSCC(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, s1.Items, 1)
	require.Len(t, s2.Items, 1)
	assert.True(t, s1.Items[0] != s2.Items[0])
	assert.Equal(t, s1.Items[0].String(), s2.Items[0].String())

	// Editing one result cannot affect the other.
	s1.Items[0].Lines[0].Items[0].Text = "CHANGED"
	assert.Equal(t, "HI", s2.Items[0].String())
}

func TestOpenFileSCC(t *testing.T) {
	s, err := astisub.OpenFile("./testdata/example-in.scc")
	require.NoError(t, err)
	require.Len(t, s.Items, 1)
	assert.Equal(t, "HELLO", s.Items[0].String())
	assert.Equal(t, sccFrameTime(37), s.Items[0].StartAt)

	// Upper-case extension behaves like the other formats.
	dir, err := ioutil.TempDir("", "astisub-scc")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, "example.SCC")
	require.NoError(t, ioutil.WriteFile(dst,
		[]byte("Scenarist_SCC V1.0\n\n00:00:01:00\t9420 9420 9140 9140 C845 4C4C 4F80 94AF 94AF\n"),
		0644))
	s2, err := astisub.OpenFile(dst)
	require.NoError(t, err)
	require.Len(t, s2.Items, 1)
	assert.Equal(t, "HELLO", s2.Items[0].String())
}

func TestReadFromSCCWriteToSRTAndWebVTT(t *testing.T) {
	s, err := astisub.ReadFromSCC(strings.NewReader(sccStream(
		sccRecord("00:00:01:00", "9420", "9420", sccPAC(1, 0), sccPAC(1, 0),
			sccWord('H', 'E'), sccWord('L', 'L'), sccWord('O', 0x00), "94AF", "94AF"),
	)))
	require.NoError(t, err)

	var srt bytes.Buffer
	require.NoError(t, s.WriteToSRT(&srt))
	assert.Contains(t, srt.String(), "HELLO")
	assert.Contains(t, srt.String(), "-->")

	var vtt bytes.Buffer
	require.NoError(t, s.WriteToWebVTT(&vtt))
	assert.Contains(t, vtt.String(), "HELLO")
	assert.Contains(t, vtt.String(), "-->")
}
