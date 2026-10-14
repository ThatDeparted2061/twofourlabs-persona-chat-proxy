// Package continuation builds the instruction the proxy sends when it moves a
// half-finished reply to another provider, and parses it back (the simulated
// upstream uses Parse to resume the same reply).
//
// The instruction goes in a trailing system message, after everything the
// provider has cached, so the cached prefix is untouched. Assistant prefill
// would be the natural tool, but current Anthropic models reject it.
package continuation

import "strings"

const (
	open  = "Your previous reply was cut off by a network error. It ended with:\n<<<"
	close = ">>>\nContinue from exactly that point, mid-word if needed. Do not repeat any of it and do not mention the interruption."
)

// Instruction asks the model to carry on after partial.
func Instruction(partial string) string { return open + partial + close }

// Parse returns the partial reply embedded in an Instruction.
func Parse(s string) (partial string, ok bool) {
	rest, ok := strings.CutPrefix(s, open)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(rest, close)
}
