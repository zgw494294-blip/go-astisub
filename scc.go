package astisub

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Scenarist_SCC V1.0 (.scc) import.
//
// An SCC file stores CEA-608 closed caption data as four hexadecimal digit
// words. Each word represents one transmitted frame of field 1 data (two
// bytes). Only caption channel 1 (CC1) is decoded: data interleaved for CC2
// is ignored while still being counted as transmission frames.
//
// References: CEA-608-E (Line 21 Data Services), the Scenarist Closed Caption
// format specification.

// Errors
var (
	ErrSCCBadHeader   = errors.New("astisub: invalid Scenarist_SCC header")
	ErrSCCBadTimecode = errors.New("astisub: invalid SCC timecode")
	ErrSCCBadWord     = errors.New("astisub: invalid SCC hexadecimal word")
)

const (
	sccHeader = "Scenarist_SCC V1.0"

	sccRows    = 15
	sccColumns = 32

	// A 29.97 frame lasts 1001/30000 seconds.
	sccFrameNanosecondsNum = 100100000 // 1001*1e9/30000 = ns numerator
	sccFrameNanosecondsDen = 3
)

// sccCaption modes
const (
	sccModeUnknown = iota
	sccModePopOn
	sccModePaintOn
	sccModeRollUp
)

// sccFrameDuration returns the wall-clock duration of frame f when the frame
// rate is 30000/1001 (29.97fps).
func sccFrameDuration(f int64) time.Duration {
	// Round f*100100000/3 to the nearest nanosecond.
	return time.Duration((f*sccFrameNanosecondsNum + 1) / sccFrameNanosecondsDen)
}

// parseSCCTimecode parses a HH:MM:SS:FF (non drop-frame) or HH:MM:SS;FF
// (drop-frame) timecode and returns the absolute number of 29.97 frames
// elapsed since 00:00:00:00.
func parseSCCTimecode(tc string) (f int64, err error) {
	if len(tc) != 11 || tc[2] != ':' && tc[2] != ';' || tc[5] != ':' && tc[5] != ';' ||
		(tc[8] != ':' && tc[8] != ';') {
		err = ErrSCCBadTimecode
		return
	}

	var hmsf [4]int
	for i, pos := range [4]int{0, 3, 6, 9} {
		c1, c2 := tc[pos], tc[pos+1]
		if c1 < '0' || c1 > '9' || c2 < '0' || c2 > '9' {
			err = ErrSCCBadTimecode
			return
		}
		hmsf[i] = int(c1-'0')*10 + int(c2-'0')
	}
	h, m, s, ff := hmsf[0], hmsf[1], hmsf[2], hmsf[3]
	if m > 59 || s > 59 || ff > 29 {
		err = ErrSCCBadTimecode
		return
	}

	dropFrame := tc[8] == ';'
	f = int64(((h*60+m)*60+s)*30 + ff)
	if dropFrame {
		// Frames 0 and 1 are dropped at the start of every minute except
		// every tenth minute.
		totalMinutes := h*60 + m
		dropped := totalMinutes - totalMinutes/10
		f -= int64(2 * dropped)
		if f < 0 || (m%10 != 0 && s == 0 && ff < 2) {
			err = ErrSCCBadTimecode
			return
		}
	}
	return
}

// Basic North American character set overrides. All other basic codes are
// plain ASCII.
var sccBasicNA = map[byte]rune{
	0x27: '\u2019', // right single quotation mark
	0x2a: '\u00e1', // á
	0x5c: '\u00e9', // é
	0x5e: '\u00ed', // í
	0x5f: '\u00f3', // ó
	0x60: '\u00fa', // ú
	0x7b: '\u00e7', // ç
	0x7c: '\u00f7', // ÷
	0x7d: '\u00d1', // Ñ
	0x7e: '\u00f1', // ñ
	0x7f: '\u2588', // full block
}

// Special North American character set: codes 0x30-0x3f prefixed by 0x11.
var sccSpecialNA = []rune{
	'\u00ae', // 0x30 ® registered sign
	'\u00b0', // 0x31 ° degree sign
	'\u00bd', // 0x32 ½ vulgar fraction one half
	'\u00bf', // 0x33 ¿ inverted question mark
	'\u2122', // 0x34 ™ trade mark sign
	'\u00a2', // 0x35 ¢ cent sign
	'\u00a3', // 0x36 £ pound sign
	'\u266a', // 0x37 ♪ eighth note
	'\u00e0', // 0x38 à
	'\u00a0', // 0x39 no-break space
	'\u00e8', // 0x3A è
	'\u00e2', // 0x3B â
	'\u00ea', // 0x3C ê
	'\u00ee', // 0x3D î
	'\u00f4', // 0x3E ô
	'\u00fb', // 0x3F û
}

// Extended Spanish/Miscellaneous and French characters: codes 0x20-0x3f
// prefixed by 0x12. They replace the previously transmitted fallback
// character.
var sccExtended1 = []rune{
	'\u00c1', // 0x20 Á
	'\u00c9', // 0x21 É
	'\u00d3', // 0x22 Ó
	'\u00da', // 0x23 Ú
	'\u00dc', // 0x24 Ü
	'\u00fc', // 0x25 ü
	'\u00b4', // 0x26 ´ acute accent
	'\u00a1', // 0x27 ¡ inverted exclamation mark
	'*',      // 0x28 asterisk
	'\u2018', // 0x29 ‘ left single quotation mark
	'-',      // 0x2A hyphen
	'\u00a9', // 0x2B © copyright sign
	'\u2120', // 0x2C ℠ service mark
	'\u2022', // 0x2D • bullet
	'\u201c', // 0x2E “ left double quotation mark
	'\u201d', // 0x2F ” right double quotation mark
	'\u00c0', // 0x30 À
	'\u00c2', // 0x31 Â
	'\u00c7', // 0x32 Ç
	'\u00c8', // 0x33 È
	'\u00ca', // 0x34 Ê
	'\u00cb', // 0x35 Ë
	'\u00eb', // 0x36 ë
	'\u00ce', // 0x37 Î
	'\u00cf', // 0x38 Ï
	'\u00ef', // 0x39 ï
	'\u00d4', // 0x3A Ô
	'\u00d9', // 0x3B Ù
	'\u00f9', // 0x3C ù
	'\u00db', // 0x3D Û
	'\u00ab', // 0x3E «
	'\u00bb', // 0x3F »
}

// Extended Portuguese/German/Danish characters: codes 0x20-0x3f prefixed by
// 0x13. They replace the previously transmitted fallback character.
var sccExtended2 = []rune{
	'\u00c3', // 0x20 Ã
	'\u00e3', // 0x21 ã
	'\u00cd', // 0x22 Í
	'\u00cc', // 0x23 Ì
	'\u00ec', // 0x24 ì
	'\u00d2', // 0x25 Ò
	'\u00f2', // 0x26 ò
	'\u00d5', // 0x27 Õ
	'\u00f5', // 0x28 õ
	'{',      // 0x29
	'}',      // 0x2A
	'\\',     // 0x2B
	'^',      // 0x2C
	'_',      // 0x2D
	'|',      // 0x2E
	'~',      // 0x2F
	'\u00c4', // 0x30 Ä
	'\u00e4', // 0x31 ä
	'\u00d6', // 0x32 Ö
	'\u00f6', // 0x33 ö
	'\u00df', // 0x34 ß
	'\u00a5', // 0x35 ¥ yen sign
	'\u00a4', // 0x36 ¤ currency sign
	'\u00a6', // 0x37 ¦ broken bar
	'\u00c5', // 0x38 Å
	'\u00e5', // 0x39 å
	'\u00d8', // 0x3A Ø
	'\u00f8', // 0x3B ø
	'\u250c', // 0x3C ┌
	'\u2510', // 0x3D ┐
	'\u2514', // 0x3E └
	'\u2518', // 0x3F ┘
}

// pacRowMap maps a preamble address code index to the 1-based screen row.
// -1 marks an invalid code. Index is ((hi<<1)&0x0e)|((lo>>5)&0x01) where hi
// and lo have their parity bit stripped.
var sccPACRowMap = [16]int{11, -1, 1, 2, 3, 4, 12, 13, 14, 15, 5, 6, 7, 8, 9, 10}

// pacIndent maps the second PAC byte masked with 0x1f to the cursor column.
var sccPACIndent = [32]int{
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, // style PACs always start at column 0
	0, 0, 4, 4, 8, 8, 12, 12,
	16, 16, 20, 20, 24, 24, 28, 28,
}

// sccScreen is one of the two CEA-608 caption memories.
type sccScreen struct {
	cell [sccRows][sccColumns]rune
	used [sccRows]bool
}

func (s *sccScreen) clear() {
	*s = sccScreen{}
}

// sccDecoder is a CC1 CEA-608 display simulator.
type sccDecoder struct {
	screen [2]sccScreen
	shown  int // index of the displayed screen

	mode  int
	rollN int // number of roll-up rows (2-4)
	row   int
	col   int

	// lastCmd is the previous CC1 control command word (parity stripped),
	// used to ignore retransmitted command pairs.
	lastCmd uint16

	items    []*Item
	cur      *Item
	curLines []string

	lastWordFrame int64 // frame of the last transmitted word, -1 if none
}

func newSCCDecoder() *sccDecoder {
	return &sccDecoder{
		mode:          sccModeUnknown,
		rollN:         2,
		row:           10,
		lastWordFrame: -1,
	}
}

// writingScreen returns the screen current edits apply to depending on the
// active mode.
func (d *sccDecoder) writingScreen() *sccScreen {
	switch d.mode {
	case sccModePopOn:
		return &d.screen[1-d.shown]
	case sccModePaintOn, sccModeRollUp:
		return &d.screen[d.shown]
	default:
		return nil
	}
}

func (d *sccDecoder) displayedScreen() *sccScreen {
	return &d.screen[d.shown]
}

// put writes a rune at an absolute position. A zero rune erases the cell.
func (d *sccDecoder) put(s *sccScreen, row, col int, r rune) {
	if s == nil || row < 0 || row >= sccRows || col < 0 || col >= sccColumns {
		return
	}
	s.cell[row][col] = r
}

// write writes a rune at the cursor position. Non-zero runes advance the
// cursor; zero runes only terminate the current row contents.
func (d *sccDecoder) write(s *sccScreen, r rune) {
	if s == nil || d.row < 0 || d.row >= sccRows || d.col < 0 || d.col >= sccColumns {
		return
	}
	s.cell[d.row][d.col] = r
	if r != 0 {
		d.col++
	}
}

// render returns the non-empty displayed rows, top to bottom, keeping
// meaningful spacing inside each row. Leading and trailing blank columns are
// omitted.
func (d *sccDecoder) render() []string {
	s := d.displayedScreen()
	var out []string
	for r := 0; r < sccRows; r++ {
		if !s.used[r] {
			continue
		}
		first, last := -1, -1
		for c := 0; c < sccColumns; c++ {
			if s.cell[r][c] != 0 {
				if first == -1 {
					first = c
				}
				last = c
			}
		}
		if first == -1 {
			continue
		}
		var b strings.Builder
		for c := first; c <= last; c++ {
			r := s.cell[r][c]
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
		out = append(out, b.String())
	}
	return out
}

func sccLinesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sync closes the current item and opens a new one whenever the visible
// screen changes at the given frame.
func (d *sccDecoder) sync(frame int64) {
	lines := d.render()
	if sccLinesEqual(lines, d.curLines) {
		return
	}
	t := sccFrameDuration(frame)
	if d.cur != nil {
		d.cur.EndAt = t
		d.items = append(d.items, d.cur)
		d.cur = nil
	}
	if len(lines) > 0 {
		d.cur = &Item{StartAt: t}
		for _, l := range lines {
			d.cur.Lines = append(d.cur.Lines, Line{Items: []LineItem{{Text: l}}})
		}
		d.curLines = lines
	} else {
		d.curLines = nil
	}
}

// finish closes the item still displayed at EOF. Hidden content is never
// shown. Remaining visible content ends on the frame following the last
// transmitted word.
func (d *sccDecoder) finish() {
	if d.cur != nil {
		d.cur.EndAt = sccFrameDuration(d.lastWordFrame + 1)
		d.items = append(d.items, d.cur)
		d.cur = nil
		d.curLines = nil
	}
	for i, it := range d.items {
		it.Index = i + 1
	}
}

// rollUp performs the roll-up carriage return on the writing screen.
func (d *sccDecoder) rollUp(s *sccScreen) {
	if s == nil || d.row < 0 {
		return
	}
	keep := d.rollN
	if d.row+1 < keep {
		keep = d.row + 1
	}
	top := d.row - keep + 1

	// Rows inside the window roll up by one row: the base row moves up,
	// and the top row of the window is lost.
	for r := top + 1; r <= d.row; r++ {
		s.cell[r-1] = s.cell[r]
		s.used[r-1] = s.used[r]
	}

	// The base row is cleared.
	var emptyRow [sccColumns]rune
	s.cell[d.row] = emptyRow
	s.used[d.row] = false

	// Anything outside the window disappears.
	for r := 0; r < sccRows; r++ {
		if r < top || r > d.row {
			s.cell[r] = emptyRow
			s.used[r] = false
		}
	}
	d.col = 0
}

// basicRune maps a basic North American code point to a rune.
func sccBasicRune(b byte) (rune, bool) {
	if b < 0x20 || b > 0x7f {
		return 0, false
	}
	if r, ok := sccBasicNA[b]; ok {
		return r, true
	}
	return rune(b), true
}

// Word categories returned by sccClassify.
const (
	sccWordIgnore  = 0 // padding, CC2 interleaved data, XDS, unknown codes
	sccWordText    = 1 // printable text (basic, special, extended)
	sccWordControl = 2 // CC1 control command (subject to retransmission)
)

// sccClassify tells whether a parity-stripped field-1 word belongs to CC1
// and whether it is text or a control command.
func sccClassify(a, b byte) int {
	switch {
	case a == 0x00:
		// No CC1 byte: the word is padding or interleaved CC2 data.
		return sccWordIgnore

	case a >= 0x20:
		// One or two basic North American characters.
		return sccWordText

	case (a == 0x10 && b >= 0x40 && b <= 0x5f) ||
		(a >= 0x11 && a <= 0x17 && b >= 0x40 && b <= 0x7f):
		// Preamble address code. The first byte ranges over 0x10-0x17 to
		// address rows 11, 1-4, 12-15 and 5-10 respectively; CC2 PACs use
		// 0x18-0x1f and are ignored.
		return sccWordControl

	case a == 0x10 && b >= 0x20 && b <= 0x2f:
		// Background attribute: no textual effect.
		return sccWordControl

	case a == 0x11 && b >= 0x20 && b <= 0x2f:
		// Mid-row style change.
		return sccWordControl

	case a == 0x11 && b >= 0x30 && b <= 0x3f:
		// Special North American character.
		return sccWordText

	case (a == 0x12 || a == 0x13) && b >= 0x20 && b <= 0x3f:
		// Extended Western European character.
		return sccWordText

	case a == 0x14 && b >= 0x20 && b <= 0x2f:
		// Miscellaneous control command.
		return sccWordControl

	case a == 0x17 && b >= 0x20 && b <= 0x23:
		// Tab offset 0-3.
		return sccWordControl

	case a == 0x17 && (b == 0x2e || b == 0x2f):
		// White italics mid-row style change.
		return sccWordControl

	default:
		// CC2 commands (0x15, 0x18-0x1f), field 2 commands, XDS
		// (0x01-0x0f) and unknown codes are ignored.
		return sccWordIgnore
	}
}

// processWord applies a single CC1 word (parity stripped) to the simulated
// display.
func (d *sccDecoder) processWord(a, b byte) {
	switch {
	case a >= 0x20:
		// One or two basic North American characters.
		s := d.writingScreen()
		if s != nil && d.row >= 0 && d.row < sccRows {
			if r, ok := sccBasicRune(a); ok {
				s.used[d.row] = true
				d.write(s, r)
			}
			if b >= 0x20 && b <= 0x7f {
				if r, ok := sccBasicRune(b); ok {
					s.used[d.row] = true
					d.write(s, r)
				}
			}
			// Row contents terminator.
			d.put(s, d.row, d.col, 0)
		}

	case (a == 0x10 && b >= 0x40 && b <= 0x5f) ||
		(a >= 0x11 && a <= 0x17 && b >= 0x40 && b <= 0x7f):
		// Preamble address code: move the cursor, never erase skipped
		// existing cells.
		index := int(((a << 1) & 0x0e) | ((b >> 5) & 0x01))
		row := sccPACRowMap[index]
		if row > 0 {
			d.row = row - 1
			d.col = sccPACIndent[b&0x1f]
		}

	case a == 0x10 && b >= 0x20 && b <= 0x2f:
		// Background attribute: no textual effect.

	case (a == 0x11 && b >= 0x20 && b <= 0x2f) ||
		(a == 0x17 && (b == 0x2e || b == 0x2f)):
		// Mid-row style change. It occupies one screen cell rendered as a
		// space.
		s := d.writingScreen()
		if s != nil && d.row >= 0 && d.row < sccRows {
			s.used[d.row] = true
			d.write(s, ' ')
			d.put(s, d.row, d.col, 0)
		}

	case a == 0x11 && b >= 0x30 && b <= 0x3f:
		// Special North American character.
		s := d.writingScreen()
		if s != nil && d.row >= 0 && d.row < sccRows {
			s.used[d.row] = true
			d.write(s, sccSpecialNA[b-0x30])
			d.put(s, d.row, d.col, 0)
		}

	case a == 0x12 && b >= 0x20 && b <= 0x3f:
		// Extended Spanish/French character replacing the previous cell.
		s := d.writingScreen()
		if s != nil && d.row >= 0 && d.row < sccRows {
			if d.col > 0 {
				d.col--
			}
			s.used[d.row] = true
			d.write(s, sccExtended1[b-0x20])
			d.put(s, d.row, d.col, 0)
		}

	case a == 0x13 && b >= 0x20 && b <= 0x3f:
		// Extended Portuguese/German/Danish character replacing the
		// previous cell.
		s := d.writingScreen()
		if s != nil && d.row >= 0 && d.row < sccRows {
			if d.col > 0 {
				d.col--
			}
			s.used[d.row] = true
			d.write(s, sccExtended2[b-0x20])
			d.put(s, d.row, d.col, 0)
		}

	case a == 0x17 && b >= 0x20 && b <= 0x23:
		// Tab offset 0-3: move the cursor without erasing skipped text.
		d.col += int(b - 0x20)
		if d.col > sccColumns {
			d.col = sccColumns
		}

	case a == 0x14 && b >= 0x20 && b <= 0x2f:
		// Miscellaneous control command.
		d.processMiscCommand(b)
	}
}

// processMiscCommand handles the second byte of a 0x14xx miscellaneous
// control command.
func (d *sccDecoder) processMiscCommand(b byte) {
	s := d.writingScreen()
	switch b {
	case 0x20: // RCL: resume caption loading (pop-on)
		d.mode = sccModePopOn
	case 0x21: // BS: backspace
		if s != nil {
			if d.col > 0 {
				d.col--
			}
			d.put(s, d.row, d.col, 0)
		}
	case 0x22, 0x23, 0x28: // AOF, AON, FON
		// No textual effect.
	case 0x24: // DER: delete to end of row
		if s != nil {
			for c := d.col; c < sccColumns; c++ {
				d.put(s, d.row, c, 0)
			}
		}
	case 0x25: // RU2
		d.mode = sccModeRollUp
		d.rollN = 2
	case 0x26: // RU3
		d.mode = sccModeRollUp
		d.rollN = 3
	case 0x27: // RU4
		d.mode = sccModeRollUp
		d.rollN = 4
	case 0x29: // RDC: resume direct captioning (paint-on)
		d.mode = sccModePaintOn
	case 0x2a, 0x2b: // RTD: text mode, treated as paint-on
		d.mode = sccModePaintOn
	case 0x2c: // EDM: erase displayed memory
		d.screen[d.shown].clear()
	case 0x2d: // CR: carriage return (roll-up)
		if d.mode == sccModeRollUp {
			d.rollUp(s)
		}
	case 0x2e: // ENM: erase non-displayed memory
		d.screen[1-d.shown].clear()
	case 0x2f: // EOC: end of caption, swap the two memories
		d.shown = 1 - d.shown
		d.col = 0
	}
}

// sccTimecodeShape returns 1 when token has the shape of a record timecode
// (HH:MM:SS:FF or HH:MM:SS;FF), 0 when it does not look like a timecode at
// all, and -1 when it looks like one but uses invalid separators or digits.
func sccTimecodeShape(token string) int {
	if len(token) != 11 {
		return 0
	}
	for _, pos := range []int{0, 1, 3, 4, 6, 7, 9, 10} {
		if token[pos] < '0' || token[pos] > '9' {
			return 0
		}
	}
	for _, pos := range []int{2, 5, 8} {
		if token[pos] != ':' && token[pos] != ';' {
			return -1
		}
	}
	return 1
}

// processWords simulates the transmission of consecutive hexadecimal words
// starting at frame base+offset. It returns the new offset within the
// current record.
func (d *sccDecoder) processWords(words []string, base int64, offset int) (int, error) {
	for i, word := range words {
		if len(word) != 4 {
			return offset, fmt.Errorf("%w: %q", ErrSCCBadWord, word)
		}
		v, err := strconv.ParseUint(word, 16, 16)
		if err != nil {
			return offset, fmt.Errorf("%w: %q", ErrSCCBadWord, word)
		}

		frame := base + int64(offset+i)
		d.lastWordFrame = frame

		// Odd parity is normal on the wire but never required to decode:
		// masking with 0x7f accepts normal files, 0x8080 padding and any
		// other odd-parity byte.
		a := byte(v>>8) & 0x7f
		b := byte(v) & 0x7f

		switch sccClassify(a, b) {
		case sccWordControl:
			// Paired retransmissions of control commands take effect once.
			// The repeated word still consumed one transmission frame,
			// which is accounted for above.
			cmd := uint16(a)<<8 | uint16(b)
			if cmd != d.lastCmd {
				d.lastCmd = cmd
				d.processWord(a, b)
			}
		case sccWordText:
			// Ordinary text is never de-duplicated, even when two
			// identical words are transmitted.
			d.lastCmd = 0
			d.processWord(a, b)
		}

		d.sync(frame)
	}
	return offset + len(words), nil
}

// ReadFromSCC parses a Scenarist_SCC V1.0 stream and returns the subtitles
// carried by caption channel 1 (CC1) of field 1.
func ReadFromSCC(i io.Reader) (o *Subtitles, err error) {
	scanner := bufio.NewScanner(i)
	// SCC records can be longer than the default scanner token size.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(bufio.ScanLines)

	// The first non-empty line must be the Scenarist header.
	var header string
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), string(BytesBOM)))
		if line == "" {
			continue
		}
		header = line
		break
	}
	if err = scanner.Err(); err != nil {
		return
	}
	if header != sccHeader {
		err = ErrSCCBadHeader
		return
	}

	d := newSCCDecoder()
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		// Every record starts with a timecode. Lines without one (some
		// files wrap long records onto extra word-only lines) are not
		// records and are ignored, like other SCC readers do.
		switch sccTimecodeShape(fields[0]) {
		case 0:
			continue
		case -1:
			err = ErrSCCBadTimecode
			return
		}

		var baseFrame int64
		if baseFrame, err = parseSCCTimecode(fields[0]); err != nil {
			return
		}
		if _, err = d.processWords(fields[1:], baseFrame, 0); err != nil {
			return
		}
	}
	if err = scanner.Err(); err != nil {
		return
	}

	d.finish()
	o = NewSubtitles()
	o.Items = d.items
	return
}
