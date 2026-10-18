# build-reproducibility Specification

## Purpose
Guarantees that local development, CI, and production container builds use the
same declared toolchains, lockfiles, and pinned base images, so a build artifact
does not change silently between environments.

## Requirements

### Requirement: Build toolchains match project manifests

Container build stages MUST use a toolchain version that satisfies the project
manifest's declared requirement (the `go` directive in `go.mod` for backend
builds; the Node major declared for frontend builds). Build stages MUST NOT rely
on an automatic toolchain download that is newer than the image tag they declare.

#### Scenario: Go builder satisfies the go.mod directive

- **WHEN** the backend image build stage declares its Go version
- **THEN** that version is greater than or equal to `go.mod`'s `go` directive and
  no hidden `GOTOOLCHAIN` download is required to compile the module

#### Scenario: Frontend builder matches the declared Node major

- **WHEN** the frontend image is built
- **THEN** its build stage runs the same Node major version declared for local
  development and CI

### Requirement: Base and runtime images are digest-pinned

Every external base image used to build or run a service image (Go builder,
Node builder, nginx runtime, Python base and uv toolchain) MUST be referenced by
immutable digest in addition to its human-readable tag.

#### Scenario: Rebuilding an unchanged commit reuses the same base images

- **WHEN** a service image is rebuilt from the same commit without an explicit
  digest bump
- **THEN** the base and runtime images resolve to the same digests as the
  previous build

### Requirement: Dependency installation is lockfile-exact

CI jobs and container builds MUST install dependencies from committed lockfiles
using deterministic commands: `npm ci` for the frontend, `uv sync --locked` for
the Python agent, and `go.sum`-verified module resolution for the backend. A job
or build MUST fail rather than silently resolve a newer version when a lockfile
is missing, stale, or out of sync with its manifest.

#### Scenario: Frontend CI install cannot update the lockfile

- **WHEN** a CI job installs frontend dependencies
- **THEN** it uses `npm ci` and fails if `package.json` and
  `frontend/package-lock.json` disagree, instead of writing a new resolution

#### Scenario: Stale Python lock fails the job

- **WHEN** `backend/agent_py/pyproject.toml` changes without regenerating
  `uv.lock`
- **THEN** the agent CI jobs fail at `uv sync --locked` instead of resolving
  fresh versions

### Requirement: Local and CI toolchain versions are declared

The repository MUST contain a checked-in declaration of the Node major version
used for frontend development and MUST document the Go toolchain version
required by `go.mod`. CI and image builds MUST match these declarations. Type
definition packages for the Node runtime MUST target the same major version as
the declared runtime.

#### Scenario: Developer and CI use the same Node major

- **WHEN** a developer follows the repository's version-file guidance
- **THEN** their Node major matches the version used by CI and the frontend
  image build

#### Scenario: Developer Go toolchain matches go.mod

- **WHEN** a developer follows the repository's local toolchain guidance
- **THEN** `go version` and `go env GOVERSION` report the version required by
  `go.mod` without relying on an automatic toolchain download

#### Scenario: Node type definitions match the runtime major

- **WHEN** frontend dependencies are resolved from the lockfile
- **THEN** the `@types/node` major matches the Node major declared for local
  development, CI, and the image build

### Requirement: Container build contexts exclude local state and secrets

Each service's Docker build context MUST exclude local virtual environments,
dependency trees, caches, test output, and environment files. Only files
required to build or run the image may be sent to the daemon.

#### Scenario: Backend context excludes the Python agent environment

- **WHEN** the backend image is built
- **THEN** the build context does not include `backend/agent_py/`, any
  `.env*` file, coverage output, or cache directories

#### Scenario: Context size is proportional to source

- **WHEN** the backend image is built from a developer tree that contains a full
  Python virtual environment
- **THEN** the reported build context remains on the order of a few megabytes
  rather than hundreds of megabytes
