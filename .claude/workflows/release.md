# Release a version

1. **Close the milestone first.** Every issue in it is closed or deliberately moved.
2. **Check the README matches what ships.** Every example, every key and every config
   option. Tick the roadmap items.
3. **Set the version** in `herdr-plugin.toml`.
4. **Run `make check`** on a clean checkout of `main`.
5. **Tag** `vX.Y.Z` and push the tag. The release workflow builds the binaries and
   checksums (#29).
6. **Write the release notes** in plain English. What's new, what's fixed and anything
   people need to do.
7. **Test the install** with `herdr plugin install swmcc/herdr-sightline` on a clean
   Mac and a clean Linux box, neither with Go installed. Check the binary against the
   published checksum.
8. **First release only:** add the `herdr-plugin` topic to the repo so it shows up in
   the herdr plugin directory.
9. **Close the milestone** and say so in the release notes.
