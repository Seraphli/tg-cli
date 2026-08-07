package markdown

import "unicode/utf8"

// runeWidth returns approximate display width (CJK = 2, others = 1).
func runeWidth(r rune) int {
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// displayWidth returns the display width of a string.
func displayWidth(s string) int {
	w := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		w += runeWidth(r)
		s = s[size:]
	}
	return w
}
