// cz-customizable config for the Nyx monorepo.
//
// Scope list mirrors the `scope-enum` rule in commitlint.config.js so
// that `git cz` produces messages commitlint will accept. Keep the two
// lists in sync when adding new scopes.
module.exports = {
  types: [
    { value: 'feat', name: 'feat:     A new feature' },
    { value: 'fix', name: 'fix:      A bug fix' },
    { value: 'docs', name: 'docs:     Documentation only changes' },
    { value: 'style', name: 'style:    Changes that do not affect the meaning of the code' },
    { value: 'refactor', name: 'refactor: A code change that neither fixes a bug nor adds a feature' },
    { value: 'perf', name: 'perf:     A code change that improves performance' },
    { value: 'test', name: 'test:     Add missing tests or correct existing tests' },
    { value: 'build', name: 'build:    Changes that affect the build system or external dependencies' },
    { value: 'ci', name: 'ci:       Changes to CI configuration files and scripts' },
    { value: 'chore', name: 'chore:    Other changes that do not modify src or test files' },
    { value: 'revert', name: 'revert:   Reverts a previous commit' },
  ],
  scopes: [
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
  scopeOverrides: {
    feat: [
      'backend',
      'frontend',
      'auth',
      'infra',
      'security',
      'docs',
    ],
  },
  allowCustomScopes: true,
  allowBreakingChanges: ['feat', 'fix'],
  subjectLimit: 100,
};
