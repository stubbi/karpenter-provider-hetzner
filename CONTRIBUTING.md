# Contributing

Thanks for your interest in improving `karpenter-provider-hetzner`. This document covers how to set up, build, test, and submit changes.

## Development environment

- Go (see the `go` directive in [`go.mod`](go.mod)).
- `make`, `git`, `docker` (for image builds), and `helm` (for chart changes).
- A Hetzner Cloud API token is only needed for end-to-end testing, not for unit tests.

The `go.mod` directive remains the minimum supported Go version. Pull requests
also build and run race and envtest tests with the exact Go version pinned in
the Docker builder (currently 1.27.3). The pinned vulnerability scan uses that
same builder version, so it checks the toolchain used for published images.

## Common tasks

```bash
make test        # run unit + controller tests with the race detector
make lint        # run golangci-lint
make generate    # regenerate the CRD and deepcopy from the Go types
make build       # build the controller binary into ./bin
make docker-build TAG=dev
```

CI runs `make test`, `golangci-lint`, `make generate-verify` (fails if generated files are stale), and the controller tests. Run these locally before opening a pull request.

## Code generation

The `HCloudNodeClass` CRD and `zz_generated.deepcopy.go` are generated from the API types in `pkg/apis/v1` by `controller-gen`. If you change those types, run `make generate` and commit the regenerated files. CI will fail if they are out of date.

## Testing conventions

- Write tests first (TDD) for new behavior.
- Tests must assert real behavior, not mock internals. The provider packages use small in-package fakes that implement the narrow hcloud client interfaces; reuse those patterns.
- Keep `go test -race ./...` green.

## Commit and PR guidelines

- Use clear, conventional-ish commit subjects (`feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `ci:`, `chore:`).
- Keep PRs focused. Separate refactors from behavior changes where practical.
- Fill out the pull request template, including how you verified the change.
- All status checks must pass before merge.

## Reporting bugs and requesting features

Use the issue templates. For security issues, follow [SECURITY.md](SECURITY.md) instead of opening a public issue.

## License

By contributing, you agree that your contributions are licensed under the [Apache 2.0 License](LICENSE).
