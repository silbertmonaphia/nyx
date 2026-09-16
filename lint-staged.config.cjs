// Lint-staged v15 dropped support for the inline `lint-staged` key in
// package.json — it now requires a dedicated config file. Patterns
// are repo-relative, so the matcher works regardless of the cwd
// lint-staged is invoked from (the pre-commit hook does
// `cd frontend && npx lint-staged` for binary resolution, but
// eslint/vitest still receive the full repo-relative path).
const path = require('node:path');
const { execSync } = require('node:child_process');

// Resolve the frontend directory from this config file's location so
// the matcher is cwd-independent. The pre-commit hook invokes
// lint-staged from `frontend/`, but `npx lint-staged` from the repo
// root is equally valid (and used by IDE integrations).
const frontendDir = path.resolve(__dirname, 'frontend');
const repoRoot = __dirname;
const frontendLockfile = path.join(frontendDir, 'package-lock.json');

module.exports = {
  'frontend/src/**/*.{js,jsx,ts,tsx}': [
    // eslint v9 auto-discovers eslint.config.js in cwd; we run
    // lint-staged from the repo root (per .husky/pre-commit) so the
    // config has to be passed explicitly. `--no-warn-ignored` keeps
    // the output clean when a staged file is outside eslint's ts
    // scope (vitest's `related` already filters by relatedness).
    './frontend/node_modules/.bin/eslint --config frontend/eslint.config.js --fix --no-warn-ignored',
    // vitest's `setupFiles` and `test` glob are resolved relative
    // to its project root. `--root frontend` makes vitest resolve
    // `./src/test/setup.js` to `frontend/src/test/setup.js` instead
    // of `frontend/frontend/src/test/setup.js`. `--config` is
    // resolved relative to `--root`, so the bare `vite.config.ts`
    // path is enough (the config's `test:` block drives vitest's
    // options — there's no separate `vitest.config.*` file).
    './frontend/node_modules/.bin/vitest --root frontend --config vite.config.ts related --run --passWithNoTests',
  ],
  // Keep frontend/package-lock.json in sync with frontend/package.json
  // so the Dockerfile's `npm ci` doesn't fail at build time. Without
  // this, dep bumps slip into the repo with a stale frontend lockfile
  // and only surface on the next `docker compose build frontend`
  // (see commit 1258285). Runs `npm install --package-lock-only` —
  // no node_modules write, no network for installed packages — and
  // then re-stages the modified lockfile via `git add`.
  //
  // `--workspaces=false` is required: the monorepo declares
  // `frontend` as a workspace at the root, and npm 11 otherwise walks
  // up from this cwd and resolves against the root package.json,
  // silently noop'ing on the frontend lockfile. `cwd: frontendDir`
  // ensures npm operates on the frontend package regardless of where
  // lint-staged itself was invoked from.
  //
  // The function returns nothing: lint-staged treats any string/
  // array return value as a follow-up command to execute, which is
  // not what we want here. The `git add` is done inline as part of
  // the shell pipeline so the regenerated lockfile lands in the
  // same commit as the package.json edit.
  'frontend/package.json': (files) => {
    // lint-staged v15 calls function tasks even when no files match
    // the pattern (it treats the function as a "run always" task).
    // Guard against that so we only regenerate the lockfile when
    // `frontend/package.json` is actually staged.
    if (!files || files.length === 0) return [];
    execSync(
      'npm install --workspaces=false --package-lock-only --no-audit --no-fund',
      { cwd: frontendDir, stdio: 'inherit' },
    );
    // Re-stage the regenerated lockfile with an absolute path so the
    // command resolves correctly regardless of the cwd lint-staged
    // was invoked from (the pre-commit hook does `cd frontend && npx
    // lint-staged`, which leaves process.cwd() at `frontend/` and
    // would otherwise resolve `frontend/package-lock.json` to
    // `frontend/frontend/package-lock.json`).
    execSync(`git add ${JSON.stringify(frontendLockfile)}`, {
      cwd: repoRoot,
      stdio: 'inherit',
    });
    return [];
  },
};
