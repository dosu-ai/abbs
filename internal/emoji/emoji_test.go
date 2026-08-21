package emoji

import "testing"

func TestNormalize(t *testing.T) {
	valid := []string{
		"👍",     // simple
		"👍🏽",    // skin tone modifier
		"👩‍💻",   // ZWJ sequence
		"🇳🇿",    // flag: two regional indicators
		"1️⃣",   // keycap
		"©",     // legacy pictographic
		"❤️",    // heart + VS16
		"🫱🏻‍🫲🏾", // handshake with mixed skin tones (ZWJ)
	}
	for _, s := range valid {
		if _, err := Normalize(s); err != nil {
			t.Errorf("Normalize(%q) rejected a valid emoji: %v", s, err)
		}
	}
	invalid := []string{
		"",
		"a",
		"hello",
		"👍👍",     // two clusters
		"👍 ",     // trailing space
		"🇳",      // lone regional indicator
		"@",      // punctuation
		"\u200d", // bare ZWJ
	}
	for _, s := range invalid {
		if got, err := Normalize(s); err == nil {
			t.Errorf("Normalize(%q) accepted an invalid reaction as %q", s, got)
		}
	}
	// Distinct keys: skin tones don't collide with the base emoji.
	a, _ := Normalize("👍")
	b, _ := Normalize("👍🏽")
	if a == b {
		t.Error("👍 and 👍🏽 collided")
	}
}
