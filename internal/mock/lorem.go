package mock

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"unicode/utf8"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// Stop reasons that the mock sends.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopToolUse   = "tool_use"
)

var loremWords = strings.Fields(`lorem ipsum dolor sit amet consectetur
adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna
aliqua enim ad minim veniam quis nostrud exercitation ullamco laboris nisi
aliquip ex ea commodo consequat duis aute irure in reprehenderit voluptate
velit esse cillum fugiat nulla pariatur excepteur sint occaecat cupidatat non
proident sunt culpa qui officia deserunt mollit anim id est laborum`)

// seed derives every random choice for one request from a hash of its body.
type seed [sha256.Size]byte

// newSeed hashes the request body without its "stream" field, so the stream
// and non-stream forms of one request carry the same reply. The keys are
// sorted on re-encode, so key order in the body does not matter either.
func newSeed(body []byte) seed {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return sha256.Sum256(body)
	}
	delete(fields, "stream")
	norm, err := json.Marshal(fields)
	if err != nil {
		return sha256.Sum256(body)
	}
	return sha256.Sum256(norm)
}

// content returns the source for the reply content. Pacing uses a separate
// source, so a change to the pace settings does not change the reply.
func (s seed) content() *rand.Rand {
	//nolint:gosec // G404: the mock needs reproducible output, not secrecy.
	return rand.New(rand.NewPCG(binary.LittleEndian.Uint64(s[0:]), binary.LittleEndian.Uint64(s[8:])))
}

func (s seed) pace() *rand.Rand {
	//nolint:gosec // G404: the mock needs reproducible output, not secrecy.
	return rand.New(rand.NewPCG(binary.LittleEndian.Uint64(s[16:]), binary.LittleEndian.Uint64(s[24:])))
}

func (s seed) id(prefix string, off int) string {
	return prefix + hex.EncodeToString(s[off:off+12])
}

// chunk is the content of one content_block_delta.
type chunk struct {
	text   string
	tokens int
}

// block is one content block of a reply.
type block struct {
	kind   string // "text" or anthropic.BlockToolUse
	id     string
	name   string
	input  []byte // the full tool input JSON
	chunks []chunk
}

// reply is the complete content of one response, decided before any byte is
// sent, so the stream and non-stream forms carry the same content.
type reply struct {
	id           string
	model        string
	blocks       []block
	stopReason   string
	inputTokens  int64
	outputTokens int64
}

func (rp *reply) add(b block) {
	for _, c := range b.chunks {
		rp.outputTokens += int64(c.tokens)
	}
	rp.blocks = append(rp.blocks, b)
}

// planReply decides the reply for req. When long is true, the reply fills
// max_tokens with text.
func planReply(req *request, sd seed, long bool) (*reply, error) {
	r := sd.content()
	rp := &reply{id: sd.id("msg_mock_", 0), model: req.Model, inputTokens: req.inputTokens()}
	limit := req.MaxTokens
	if long {
		rp.add(textBlock(r, limit))
		rp.stopReason = stopMaxTokens
		return rp, nil
	}
	t, err := pickTool(r, req)
	if err != nil {
		return nil, err
	}
	if t == nil {
		n := 20 + r.IntN(101)
		rp.stopReason = stopEndTurn
		if n > limit {
			n, rp.stopReason = limit, stopMaxTokens
		}
		rp.add(textBlock(r, n))
		return rp, nil
	}
	n := min(5+r.IntN(11), limit)
	rp.add(textBlock(r, n))
	args, err := toolArgs(r, t.InputSchema)
	if err != nil {
		return nil, err
	}
	chunks := jsonChunks(r, args)
	// A tool call that does not fit in max_tokens is left out rather than
	// cut, so that no reply carries invalid input JSON.
	if len(chunks) > limit-n {
		rp.stopReason = stopMaxTokens
		return rp, nil
	}
	rp.add(block{kind: anthropic.BlockToolUse, id: sd.id("toolu_mock_", 12), name: t.Name, input: args, chunks: chunks})
	rp.stopReason = stopToolUse
	return rp, nil
}

// textBlock returns n words of lorem ipsum in deltas of 1 to 4 words, split
// into sentences of 6 to 15 words.
func textBlock(r *rand.Rand, n int) block {
	b := block{kind: "text"}
	left := 0 // words left in the current sentence
	for i := 0; i < n; {
		k := min(1+r.IntN(4), n-i)
		var sb strings.Builder
		for j := range k {
			w := loremWords[r.IntN(len(loremWords))]
			if left == 0 {
				w = strings.ToUpper(w[:1]) + w[1:]
				left = 6 + r.IntN(10)
			}
			left--
			if i+j > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(w)
			if left == 0 || i+j == n-1 {
				sb.WriteByte('.')
				left = 0
			}
		}
		b.chunks = append(b.chunks, chunk{text: sb.String(), tokens: k})
		i += k
	}
	return b
}

// jsonChunks splits s into partial_json pieces of 3 to 10 bytes. A cut never
// falls inside a UTF-8 sequence.
func jsonChunks(r *rand.Rand, s []byte) []chunk {
	var out []chunk
	for len(s) > 0 {
		i := min(3+r.IntN(8), len(s))
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}
		out = append(out, chunk{text: string(s[:i]), tokens: 1})
		s = s[i:]
	}
	return out
}

func (b *block) text() string {
	var sb strings.Builder
	for _, c := range b.chunks {
		sb.WriteString(c.text)
	}
	return sb.String()
}
