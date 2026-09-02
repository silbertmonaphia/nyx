// Commitlint config — enforces Conventional Commits for semantic-release.
//
// Scope list mirrors the scopes already in use across the repo's history
// (backend, frontend, auth, infra, security, ci, docs, claude, env,
// observability, data, repo). Add new scopes here when they appear so
// devs get fast feedback instead of a release-tool surprise later.
//
// Merge commits are ignored: they have no useful type for the analyzer
// and would otherwise bump versions on every PR.
module.exports = {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'scope-enum': [
      2,
      'always',
      [
        'backend',
        'frontend',
        'auth',
        'infra',
        'security',
        'ci',
        'docs',
        'claude',
        'env',
        'observability',
        'data',
        'repo',
      ],
    ],
    'scope-empty': [0],
    'type-empty': [2, 'never'],
    'subject-empty': [2, 'never'],
    'header-max-length': [2, 'always', 100],
  },
  ignores: [(message) => /^Merge branch/.test(message) || /^Revert /.test(message)],
};
