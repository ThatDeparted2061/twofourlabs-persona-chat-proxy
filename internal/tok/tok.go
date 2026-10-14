// Package tok estimates token counts.
package tok

// Count estimates tokens as one per four bytes, rounded up.
// ponytail: byte heuristic, within ~10-15% of BPE tokenizers on English chat.
// Swap in a real tokenizer if budgets need to match provider bills exactly;
// cost accounting always uses the usage numbers the provider reports.
func Count(s string) int { return (len(s) + 3) / 4 }
