# Pony versioning and releases

Pony uses Git tags in Go module version format: `vMAJOR.MINOR.PATCH`.
The existing `v0.1.0` tag marks the first published version. The repository
is still pre-1.0, so minor versions can add features and may include
incompatible changes. Use patch versions for fixes that do not add a feature.

Examples:

- `v0.1.1` — a bug fix to the `v0.1` line.
- `v0.2.0` — the next feature release.
- `v1.0.0` — the first release that promises compatibility for the public CLI
  and any public Go packages.

## Cut a release

1. Finish and commit the changes intended for the release.
2. Choose the next version and create an annotated tag on that commit. For
   example:

   ```sh
   git tag -a v0.2.0 -m "Pony v0.2.0"
   git push origin v0.2.0
   ```

3. Users can install the newest tagged release with:

   ```sh
   go install github.com/akansha204/pony/cmd/pony@latest
   ```

   Or install that exact release with:

   ```sh
   go install github.com/akansha204/pony/cmd/pony@v0.2.0
   ```

Go resolves `@latest` from module versions, so pushing commits to `master`
alone does not make them the latest installable release. Push a new version
tag when the release is ready.

## Check the installed build

```sh
pony --version
```

Versioned `go install` builds report the module version. `make build` and
`make run` report the version derived from the local Git checkout, including
the commit when the checkout is between tags and a `-dirty` suffix when there
are uncommitted changes.
