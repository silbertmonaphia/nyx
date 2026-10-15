// semantic-release config for the Nyx monorepo.
//
// Two independent release streams: `backend` and `frontend`, each with
// its own version + CHANGELOG.md. Tag format `${package}@${version}`
// yields tags like `backend@0.4.0` and `frontend@0.4.1`.
//
// @semantic-release/monorepo fans the plugin pipeline out per workspace
// package; the `workspaces` field in package.json tells it which dirs
// are packages. It uses the `package.json` `name` field as the package
// identifier (so backend/package.json must have `"name": "backend"` and
// frontend/package.json `"name": "frontend"`).
//
// Routing of commits to packages is PATH-based, not message-scope-based:
// commits that touched only `backend/**` produce a backend release; only
// `frontend/**` produces a frontend release. The message scope is kept
// for human readability but doesn't gate which package releases.
//
// No @semantic-release/npm is configured — neither package is published
// to the npm registry; both keep `"private": true`.
const path = require('path');

module.exports = {
  branches: [
    'main',
    { name: 'mvp', channel: 'mvp' },
  ],
  tagFormat: '${package}@${version}',

  plugins: [
    [
      '@semantic-release/commit-analyzer',
      {
        preset: 'angular',
        releaseRules: [
          { type: 'breaking', release: 'major' },
          { type: 'feat', release: 'minor' },
          { type: 'fix', release: 'patch' },
          { type: 'perf', release: 'patch' },
          { type: 'refactor', release: 'patch' },
          { type: 'docs', release: false },
          { type: 'chore', release: false },
          { type: 'style', release: false },
          { type: 'test', release: false },
          { type: 'build', release: false },
          { type: 'ci', release: false },
          { scope: 'claude', release: false },
          { subject: '^Merge ', release: false },
        ],
        parserOpts: {
          noteKeywords: ['BREAKING CHANGE', 'BREAKING-CHANGE'],
        },
      },
    ],
    '@semantic-release/release-notes-generator',
    [
      '@semantic-release/changelog',
      {
        // Per-package changelog: backend/CHANGELOG.md, frontend/CHANGELOG.md.
        changelogFile: (pkg) => path.join(pkg.name, 'CHANGELOG.md'),
      },
    ],
    [
      '@semantic-release/exec',
      {
        // Runs once per package release. Bumps the in-source version
        // references so the next build stamps them correctly.
        prepareCmd: './scripts/bump-version.sh ${nextRelease.version}',
      },
    ],
    [
      '@semantic-release/git',
      {
        assets: [
          'backend/CHANGELOG.md',
          'frontend/CHANGELOG.md',
          'backend/internal/platform/observability/tracing.go',
          'frontend/package.json',
          'package.json',
          'package-lock.json',
        ],
        message: 'release: ${nextRelease.gitTag}\n\n${nextRelease.notes}',
      },
    ],
  ],
};
