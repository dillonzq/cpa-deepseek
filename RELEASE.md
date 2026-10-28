# Release procedure

The first version is **v0.1.0**. Release tags use `vMAJOR.MINOR.PATCH` with numeric components only; the plugin metadata stores the version without `v`. Release assets follow the [CLIProxyAPI Plugins Store requirements](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements).

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

3. Review the ZIP and confirm the embedded plugin registration reports `0.1.0`. Local packaging targets the current Go OS/architecture and requires CGO, its C compiler, and Python 3. CI builds all five supported targets on native runners. Branch and pull request builds use the placeholder version `0.0.0`.
4. Commit the release preparation, push `main`, and confirm the Build workflow passes for that commit.

## Create and publish

1. Tag the verified commit and push the tag:

   ```bash
   git tag -a v0.1.0 -m 'Release v0.1.0'
   git push origin v0.1.0
   ```

2. The tag workflow reruns all checks, builds five ZIPs, validates their layout, and creates a **draft** GitHub Release with the release notes and `checksums.txt`. Nothing is published automatically. A rerun can replace draft assets, but refuses to modify a published release.
3. Download the draft assets and verify `sha256sum --check checksums.txt` (Linux), `shasum -a 256 -c checksums.txt` (macOS), or compare `Get-FileHash -Algorithm SHA256` results (PowerShell). When downloading only one ZIP, check its matching entry.
4. Review the release notes and attachments. Publish the draft explicitly when ready.

Each ZIP is named `cpa-deepseek_<version>_<goos>_<goarch>.zip`, with no leading `v` in the filename version. For example, `v0.1.0` includes `cpa-deepseek_0.1.0_linux_amd64.zip`. The single target dynamic library is at the ZIP root: `cpa-deepseek.so` (Linux), `cpa-deepseek.dylib` (macOS), or `cpa-deepseek.dll` (Windows). The generated C header, both READMEs, example configuration, MIT license, and icon source notes are also included, without a versioned wrapper directory.

`checksums.txt` contains one SHA-256 entry per ZIP in sha256sum format (`<sha256>  <filename>`). Local packaging creates only the current platform ZIP; the release job generates the combined checksum file after collecting all five platforms.
