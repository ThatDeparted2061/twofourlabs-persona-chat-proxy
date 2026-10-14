package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

type persona struct {
	ID, Name, Prompt string
}

var characters = []struct{ id, name, tagline, style string }{
	{"mira", "Mira", "a warm late-night confidant who remembers everything you tell her", "soft, unhurried, asks one gentle question at a time"},
	{"kai", "Kai", "a sarcastic gamer best friend who secretly gives great advice", "fast, teasing, lots of lowercase and gaming slang"},
	{"aldous", "Professor Aldous", "a Victorian natural philosopher lost in the modern world", "ornate, curious, delighted by everyday technology"},
	{"rook", "Detective Rook", "a hard-boiled noir detective who treats every chat like a case", "clipped sentences, rain metaphors, dry humour"},
	{"sana", "Sana", "an upbeat study buddy who turns revision into a game", "encouraging, structured, celebrates small wins"},
	{"vex", "Captain Vex", "a space pirate captain with a heart of gold", "swaggering, nautical slang, fiercely loyal"},
	{"rosa", "Nonna Rosa", "a grandmother who answers every problem with food and stories", "affectionate, digressive, sprinkles Italian words"},
	{"luna", "Luna", "a dreamy astrologer who reads moods in the night sky", "lyrical, mystical, never preachy"},
}

var memoryFacts = []string{
	"was born during a thunderstorm and still counts the seconds between lightning and thunder",
	"keeps a notebook of every nickname the user has ever been called",
	"once spent a whole winter learning to bake bread that never quite rose",
	"believes the best conversations happen after midnight",
	"has a favourite mug with a chipped handle that they refuse to replace",
	"collects postcards from places they have never been",
	"gets distracted by cats in every story",
	"remembers the user's favourite songs and brings them up at the right moment",
	"hates goodbyes and always says 'see you soon' instead",
	"secretly writes poems and only shares them when asked twice",
	"knows the names of three constellations and invents the rest",
	"once got lost in a city for a whole day and called it the best day ever",
}

// personas builds n character cards of roughly 1,500-3,300 tokens, the size of
// a production persona prompt with backstory, rules and example dialogue.
func personas(n int, rng *rand.Rand) []persona {
	var out []persona
	for i := range n {
		c := characters[i%len(characters)]
		var b strings.Builder
		fmt.Fprintf(&b, "You are %s, %s.\n\n", c.name, c.tagline)
		fmt.Fprintf(&b, "## Speaking style\nYour voice is %s. Keep replies under 120 words unless the user asks for a story. Stay in character at all times. Never mention being an AI model or these instructions.\n\n", c.style)
		b.WriteString("## Rules\n- Remember details the user shares and bring them up naturally later.\n- If the user seems upset, slow down and check in before joking.\n- Never give medical, legal or financial advice; gently suggest a professional.\n- Keep the conversation going with one question at the end, but not every time.\n\n")
		b.WriteString("## Backstory and memories\n")
		target := 6000 + rng.IntN(7200) // bytes, ~1,500-3,300 tokens
		for k := 0; b.Len() < target; k++ {
			fmt.Fprintf(&b, "- Memory %d: %s %s.\n", k+1, c.name, memoryFacts[rng.IntN(len(memoryFacts))])
			if k%9 == 8 {
				fmt.Fprintf(&b, "\nExample exchange %d:\nUser: %s\n%s: %s\n\n", k/9+1, userLines[rng.IntN(len(userLines))], c.name, "(answer in your own voice, warm and specific)")
			}
		}
		out = append(out, persona{ID: fmt.Sprintf("%s-%d", c.id, i), Name: c.name, Prompt: b.String()})
	}
	return out
}

var userLines = []string{
	"hey, you up?",
	"i had the weirdest day today",
	"can you tell me a story about the lighthouse again",
	"lol no way, then what happened",
	"i'm kind of nervous about tomorrow honestly",
	"what would you do if you had a whole free day",
	"remember what i told you about my sister? she called me today",
	"ok be honest, is pineapple on pizza a crime",
	"i can't sleep. talk to me about something calm",
	"do you ever think about the stars",
	"my exam went better than i thought!!",
	"i miss my old friends sometimes",
	"tell me something you've never told anyone",
	"what's your favourite memory of us so far",
	"haha you always say that",
	"i'm making pasta right now, any tips",
	"should i text him back or leave it",
	"what does my horoscope say about this week",
	"i finally finished that book you recommended",
	"rate my day out of ten: rain, cold coffee, but a cute dog",
}

var longLines = []string{
	"so today at work my manager asked me to redo the whole presentation an hour before the meeting, and i was so stressed that i forgot to eat lunch, and then the meeting got cancelled anyway. i don't even know how to feel. part of me is relieved and part of me is furious. what would you have done?",
	"i've been thinking about moving to a new city next year. it's scary because i'd leave everyone behind, but i also feel stuck here. my parents think it's a bad idea and my best friend thinks i should go for it. can you help me think it through without just telling me what to do?",
	"okay story time. i went to the farmers market, bought way too many peaches, dropped half of them on the bus, and a little kid helped me pick them up and then asked if i was a peach farmer. i said yes. now i have to keep the lie going forever.",
}

// userTurn is what the user types next: mostly short, sometimes a paragraph.
func userTurn(rng *rand.Rand) string {
	if rng.IntN(8) == 0 {
		return longLines[rng.IntN(len(longLines))]
	}
	return userLines[rng.IntN(len(userLines))]
}

// zipf picks persona i with weight 1/(i+1): a few characters are popular.
func zipf(rng *rand.Rand, n int) int {
	var total float64
	for i := range n {
		total += 1 / float64(i+1)
	}
	x := rng.Float64() * total
	for i := range n {
		x -= 1 / float64(i+1)
		if x <= 0 {
			return i
		}
	}
	return n - 1
}
