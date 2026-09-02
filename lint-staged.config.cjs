// Lint-staged v15 dropped support for the inline `lint-staged` key in
// package.json — it now requires a dedicated config file. Patterns
// are repo-relative, so the matcher works regardless of the cwd
// lint-staged is invoked from (the pre-commit hook does
// `cd frontend && npx lint-staged` for binary resolution, but
// eslint/vitest still receive the full repo-relative path).
module.exports = {
  'frontend/src/**/*.{js,jsx,ts,tsx}': [
    'eslint --fix',
    'vitest related --run --passWithNoTests',
  ],
};
