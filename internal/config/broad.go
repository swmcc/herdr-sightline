package config

import (
	"regexp"
	"slices"
)

// ordinaryOutput is healthy output that no rule should match. An ignore rule
// that does would hide far more than it was written for, and an error pattern
// that does would cry wolf on every line.
var ordinaryOutput = []string{
	" ", "a", "0", "ok", "{}",
	"GET / 200",
	"ready in 312 ms",
	"compiled successfully",
	"Listening on http://localhost:3000",
	"INFO server started",
	"Tests: 4 passed",
}

// commonErrors are the shapes of error almost every stack prints. An ignore
// rule that matches one hides a whole kind of error, not the one it was for.
var commonErrors = []string{"Error: boom", "panic: boom", "TypeError: boom", "error: boom"}

// OrdinaryOutput returns the healthy lines a rule must never match.
func OrdinaryOutput() []string { return slices.Clone(ordinaryOutput) }

// CommonErrors returns errors an ignore rule must never match.
func CommonErrors() []string { return slices.Clone(commonErrors) }

// MatchesOrdinary reports whether re matches an empty line or ordinary output,
// and which line. An empty line comes back as "" with broad set, since nothing
// else can.
func MatchesOrdinary(re *regexp.Regexp) (line string, broad bool) {
	if re.MatchString("") {
		return "", true
	}
	for _, l := range ordinaryOutput {
		if re.MatchString(l) {
			return l, true
		}
	}
	return "", false
}
