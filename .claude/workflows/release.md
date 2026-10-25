# Release a version

1. **Close the milestone first.** Every issue in it is closed or deliberately moved.
2. **Check the README matches what ships.** Every example, every key and every config
   option. Tick the roadmap items.
3. **Set the version** in `herdr-plugin.toml`.
4. **Run `make check`** on a clean checkout of `main`.
5. **Tag** `vX.Y.Z` and push the tag. The tag must match the version in
   `herdr-plugin.toml` or the release workflow stops. It runs the tests, then
   GoReleaser builds macOS and Linux binaries for amd64 and arm64 and publishes them
   with `checksums.txt`. To try the build locally first, run
   `goreleaser release --snapshot --clean` and look in `dist/`.
6. **Write the release notes** in plain English. What's new, what's fixed and anything
   people need to do.
7. **Test the install** with `herdr plugin install swmcc/herdr-sightline` on a clean
   Mac and a clean Linux box, neither with Go installed. The install prints the sha256
   it checked. Compare it with `checksums.txt` on the release. Then check
   `herdr plugin update swmcc.sightline` picks the new version up on a machine that had
   the old one.
8. **First release only:** add the `herdr-plugin` topic to the repo so it shows up in
   the herdr plugin directory.
9. **Close the milestone** and say so in the release notes.
