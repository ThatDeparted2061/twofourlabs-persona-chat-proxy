package fakellm

import (
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

var vocab = strings.Fields(`the moon was quiet over the harbor tonight and I kept thinking
about what you said earlier honestly it made me smile you always notice the small
things like rain on the window or the smell of old books I wanted to tell you a
story about the lighthouse keeper who painted every storm he survived maybe we could
walk there tomorrow if the fog lifts I would bring tea and that terrible playlist
you pretend to hate tell me more about your day was it heavy or light I am here
either way and I am not going anywhere sometimes the best thing is to just sit
with a feeling until it softens do you remember the first song you ever loved
mine was hummed by my grandmother while she cooked`)

// Reply is the full answer the simulated model gives for a conversation. It
// depends only on the persona and the last user message, so a provider taking
// over mid-stream regenerates the same text and can resume it. It ends with a
// trailer "[#<words>:<crc32>]" that Verify checks, so a client can prove a
// streamed reply arrived with nothing dropped or duplicated.
func Reply(persona, lastUser string, maxTokens int) string {
	h := fnv.New64a()
	h.Write([]byte(persona))
	h.Write([]byte{0})
	h.Write([]byte(lastUser))
	seed := h.Sum64()
	rng := rand.New(rand.NewPCG(seed, seed>>7|1))

	target := 40 + rng.IntN(180) // tokens
	if maxTokens > 0 && target > maxTokens-6 {
		target = max(4, maxTokens-6)
	}
	var b strings.Builder
	words := 0
	for (b.Len()+3)/4 < target {
		if words > 0 {
			b.WriteByte(' ')
		}
		w := vocab[rng.IntN(len(vocab))]
		if words%11 == 10 {
			w += "."
		}
		b.WriteString(w)
		words++
	}
	body := b.String()
	return fmt.Sprintf("%s [#%d:%08x]", body, words, crc32.ChecksumIEEE([]byte(body)))
}

var trailer = regexp.MustCompile(`(?s)^(.*) \[#(\d+):([0-9a-f]{8})\]$`)

// Verify reports whether text is a complete, unaltered Reply.
func Verify(text string) bool {
	m := trailer.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[2])
	return len(strings.Fields(m[1])) == n && fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(m[1]))) == m[3]
}

// pieces splits s into word-sized stream chunks that concatenate back to s.
func pieces(s string) []string { return strings.SplitAfter(s, " ") }
