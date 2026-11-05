package mock

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestTextBlock(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, n := range []int{1, 2, 7, 64, 500} {
		b := textBlock(r, n)
		text := b.text()
		if got := len(strings.Fields(text)); got != n {
			t.Fatalf("n=%d: %d words", n, got)
		}
		tokens := 0
		for _, c := range b.chunks {
			if c.tokens < 1 || c.tokens > 4 || len(strings.Fields(c.text)) != c.tokens {
				t.Fatalf("chunk %q counts %d tokens", c.text, c.tokens)
			}
			tokens += c.tokens
		}
		if tokens != n {
			t.Fatalf("n=%d: chunks count %d tokens", n, tokens)
		}
		first, _ := utf8.DecodeRuneInString(text)
		if !unicode.IsUpper(first) || !strings.HasSuffix(text, ".") || strings.HasSuffix(text, "..") {
			t.Fatalf("n=%d: text %q is not a sentence", n, text)
		}
	}
}

func TestJSONChunks(t *testing.T) {
	in := []byte(`{"text":"héllo wörld ✓ ✓ ✓"}`)
	r := rand.New(rand.NewPCG(3, 4))
	var joined strings.Builder
	for _, c := range jsonChunks(r, in) {
		if !utf8.ValidString(c.text) {
			t.Fatalf("chunk %q splits a UTF-8 sequence", c.text)
		}
		joined.WriteString(c.text)
	}
	if joined.String() != string(in) {
		t.Fatalf("chunks join to %q", joined.String())
	}
}
