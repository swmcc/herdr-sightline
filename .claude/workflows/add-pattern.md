# Add an error pattern

Patterns decide what Sightline calls an error. A bad one either misses real failures
or cries wolf, and crying wolf is worse.

1. **Get real output.** A pattern starts from output someone actually saw, not from
   memory. Strip anything private before it goes in the repo.
2. **Add a match fixture.** Save the failing output in
   `internal/engine/testdata/patterns/<preset>/<name>.match.txt`.
3. **Add a no-match fixture.** Save healthy output from the same tool that looks
   close but isn't an error, in `<name>.nomatch.txt`. Warnings, deprecation notices
   and log lines that mention the word "error" are good candidates.
4. **Write the pattern** in RE2 syntax. Anchor it where you can. Prefer matching the
   shape of the error (a stack frame, a file and line) over a single word.
5. **Run the tests.** Both fixtures must pass. Then run the whole pattern suite to make
   sure the new pattern doesn't match another preset's healthy output.
6. **Check grouping.** Two runs of the same error with different timestamps or ports
   should produce the same fingerprint.

Don't merge a pattern without both fixtures. If you can't find healthy output that
gets close, the pattern is probably too broad.
