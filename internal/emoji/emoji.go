// Package emoji validates reaction emoji per the spec: exactly one Unicode
// emoji, meaning one extended grapheme cluster whose base is an emoji. ZWJ
// sequences (👩‍💻), skin-tone modifiers (👍🏽), flags (🇳🇿), and keycaps
// (1️⃣) are single clusters of multiple code points — segmentation uses
// rivo/uniseg, never a codepoint regex. The normalized (NFC) cluster is the
// canonical key, so visually identical sequences don't fragment tallies.
package emoji

import (
	"errors"
	"unicode"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

var ErrInvalid = errors.New("not a single emoji grapheme cluster")

// extendedPictographic approximates Unicode's Extended_Pictographic
// property: the blocks that hold emoji bases. Variation selectors, ZWJ,
// modifiers, and tags ride along inside the same grapheme cluster and are
// accepted implicitly.
var extendedPictographic = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0x00A9, 0x00A9, 1}, // ©
		{0x00AE, 0x00AE, 1}, // ®
		{0x203C, 0x203C, 1},
		{0x2049, 0x2049, 1},
		{0x2122, 0x2122, 1},
		{0x2139, 0x2139, 1},
		{0x2194, 0x21AA, 1},
		{0x231A, 0x231B, 1},
		{0x2328, 0x2328, 1},
		{0x23CF, 0x23CF, 1},
		{0x23E9, 0x23FA, 1},
		{0x24C2, 0x24C2, 1},
		{0x25AA, 0x25AB, 1},
		{0x25B6, 0x25B6, 1},
		{0x25C0, 0x25C0, 1},
		{0x25FB, 0x25FE, 1},
		{0x2600, 0x27BF, 1}, // misc symbols, dingbats
		{0x2934, 0x2935, 1},
		{0x2B05, 0x2B07, 1},
		{0x2B1B, 0x2B1C, 1},
		{0x2B50, 0x2B50, 1},
		{0x2B55, 0x2B55, 1},
		{0x3030, 0x3030, 1},
		{0x303D, 0x303D, 1},
		{0x3297, 0x3297, 1},
		{0x3299, 0x3299, 1},
	},
	R32: []unicode.Range32{
		{0x1F000, 0x1FBFF, 1}, // the emoji planes: symbols, pictographs, ext-A/B
	},
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// Normalize validates s as exactly one emoji and returns the canonical
// (NFC-normalized) cluster to use as the storage key.
func Normalize(s string) (string, error) {
	s = norm.NFC.String(s)
	if s == "" || uniseg.GraphemeClusterCount(s) != 1 {
		return "", ErrInvalid
	}
	runes := []rune(s)
	base := runes[0]
	switch {
	case isRegionalIndicator(base):
		// Checked before the pictographic table (regional indicators live
		// inside its range): a flag is exactly two regional indicators — a
		// lone one is not an emoji.
		if len(runes) == 2 && isRegionalIndicator(runes[1]) {
			return s, nil
		}
		return "", ErrInvalid
	case unicode.Is(extendedPictographic, base):
		return s, nil
	case len(runes) > 1 && runes[len(runes)-1] == 0x20E3:
		// Keycap sequence: base char + (VS16) + U+20E3.
		return s, nil
	default:
		return "", ErrInvalid
	}
}
