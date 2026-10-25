# Work an issue

Take one issue from open to a pull request.

1. **Read the issue.** `gh issue view <n>`. Read the linked issues too. If it's
   labelled `💭 needs design` and the design isn't settled in the thread, stop and ask.
2. **Check the README.** It's the spec. Note anything in the issue that disagrees with
   it and raise it rather than quietly picking one.
3. **Branch from an up to date `main`.** `git switch main && git pull --rebase`, then
   `git switch -c <n>-<short-name>`.
4. **Write the tests first** where it makes sense. Table driven, fixtures in `testdata/`.
5. **Implement.** Keep the engine free of herdr. Keep the change to what the issue asks.
6. **Run `make check`.** Fix anything it finds.
7. **Review.** Use [reviews/go.md](../reviews/go.md). If the change touches the
   message format or delivery, also use [reviews/injection.md](../reviews/injection.md).
8. **Update the README** if behaviour changed, and tick the issue's roadmap item.
9. **Commit** with an emoji message. No AI attribution.
10. **Rebase on `main`, push and open the PR.** Title in plain English, body says what
    changed and why, ending with `Closes #<n>`.
11. **Label the PR.** Give it the same labels and milestone as the issue, plus any
    others that fit what actually changed. `gh pr edit <pr> --add-label "<label>" --milestone "<milestone>"`.
12. **Wait for CI.** The `test` and `issue-link` checks have to pass before the PR can merge.
