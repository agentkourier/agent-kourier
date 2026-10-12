# Security checks

`govulncheck.sh` checks a Go module. `scan-image.sh` checks one image architecture and blocks
high and critical vulnerabilities, including findings without a fix. `scan-secrets.sh` checks Git
history with redacted output. Scanner errors fail the command.

Renovate updates module dependencies, Actions, documentation requirements and shell-held container
pins. Trivy and Gitleaks installers require a reviewed manual update because their versions,
platform asset names and SHA-256 checksums must change together:

1. Choose the upstream release in the tool's official repository.
2. Update the version, all four platform asset names and their SHA-256 checksums in the installer.
3. Run `hack/security/test.sh`, which checks that every asset belongs to the selected version.
4. Run the installer on the current host and verify the tool's reported version. The installer
   downloads over HTTPS and rejects an archive whose checksum does not match the pinned value.
5. Review the complete change before merging it. CI exercises the Linux AMD64 installer; verify
   the remaining platform checksums against the same upstream release before approval.

Historical test-canary exceptions are individual Gitleaks fingerprints. Do not exclude a file
or directory. A new finding needs inspection before adding an exception, even in a test file.
Never print a suspected secret while inspecting a report.
