# Release procedure

The first version is **v0.1.0**. Release tags use `vMAJOR.MINOR.PATCH`; the plugin metadata stores the version without `v`.

## Prepare

1. Add or update `docs/releases/<tag>.md` with user-facing changes, compatibility requirements and known limits.
2. Run the same checks as CI with the Go toolchain in `go.mod`:

   ```bash
   go mod download all
   test -z "$(gofmt -l main.go internal tests)"
   go test ./...
   go test -race ./internal/...
   go vet -tags debug ./...
   go test -tags debug ./...
   bash scripts/package.sh v0.1.0
   ```

3. Review the archive and confirm the embedded plugin registration reports `0.1.0`. Local packaging targets the current Go OS/architecture and requires CGO and its C compiler. CI builds all five supported targets on native runners.
4. Commit the release preparation, push `main`, and confirm the Build workflow passes for that commit.

## Create and publish

1. Tag the verified commit and push the tag:

   ```bash
   git tag -a v0.1.0 -m 'Release v0.1.0'
   git push origin v0.1.0
   ```

2. The tag workflow reruns all checks, builds five archives, and creates a **draft** GitHub Release with the release notes and `SHA256SUMS`. Nothing is published automatically. A rerun can replace draft assets, but refuses to modify a published release.
3. Download the draft assets and verify `sha256sum --check SHA256SUMS` (Linux), `shasum -a 256 -c SHA256SUMS` (macOS), or compare `Get-FileHash -Algorithm SHA256` results (PowerShell). When downloading only one archive, check its matching entry.
4. Review the release notes and attachments. Publish the draft explicitly when ready.

Each archive contains a versioned directory with the dynamic library, generated C header, both READMEs, example configuration, MIT license, and icon source notes. Unix archives use `.tar.gz`; Windows uses `.zip`.
