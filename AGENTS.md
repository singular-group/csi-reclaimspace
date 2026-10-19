# AGENTS.md

How this repo is built and why. Applies to people and agents alike.

## What it is

One Go binary that serves the CSI-Addons Identity and ReclaimSpace gRPC
services next to a CSI driver. It does one thing — FITRIM on the volume's
staging path — and stays small. New features need a concrete user first.

## Rules

- English everywhere. Comments say why, in one line; no history.
- Dependencies: grpc, csi-addons/spec, x/sys. Standard library for tests
  and logging. A new dependency needs a reason in the PR.
- Fail fast: startup validates its inputs and exits non-zero with the reason;
  Kubernetes does the retrying. No retry loops in the binary.
- Never trim a path that is not a mount root (as `statx` reports it): an
  unmounted staging path would trim the node's own filesystem.
- Every behaviour has a test: unit tests beside the code, the real-kernel
  path in `test/` (`go test -tags functional ./test/`, as root), and the
  whole chain against the CSI-Addons controller in `test/e2e/run.sh <image>`
  (docker, kind and kubectl).
- No references to any particular deployment, company or infrastructure:
  this repo is generic and public.

## Versions

Image versions are derived from git, never written by hand:
`<x.y.z tag>.<commits since it>`, counted at the last commit that changed the
image sources (`main.go`, `internal/`, `go.mod`, `go.sum`, `Dockerfile`). CI
builds each version once on merge to `main` and pushes `:<version>` and
`:latest`; a published version is never overwritten. A new `x.y` is the only
manual act: `git tag -a 1.1.0 -m '...' && git push origin 1.1.0`.

## Workflow

Branch from `main` (`feat/…`, `fix/…`, `test/…`, `docs/…`, `ci/…`), open a
PR, merge when CI is green. Conventional Commits with a scope; the subject says
what and why.
