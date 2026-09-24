// Architecture boundary linter — see Harness.md #2.
//
// Rules enforced:
//   1. no-cross-feature-<a>-to-<b>: a file inside features/<a> MUST NOT
//      import from features/<b> where b != a. Features are siblings.
//      One rule per (from, to) feature pair, generated at config load
//      time from the filesystem so a new feature is covered without
//      hand-editing this file.
//   2. shared-not-from-features: shared infra (components/, hooks/,
//      services/, store/, api/, utils/, types/, assets/) MUST NOT import
//      from features/. Shared is upstream of features.
//
// Why a generated rule list instead of `to.pathNot` with a backreference?
// dependency-cruiser v18 does not expand cross-field captures in
// `pathNot`, and predicate functions in `to` aren't serializable
// (config is cloned). Static per-pair rules are explicit and cheap.
//
// `services/api.ts` is shared (not a feature) and is the canonical HTTP
// client — every feature imports it. That's the intended seam.
//
// Tighten further with severity overrides as patterns stabilise; start
// narrow so we don't trip existing code while we wire the gate.

const fs = require('node:fs');
const path = require('node:path');

const FEATURES_DIR = path.join(__dirname, 'src', 'features');

function discoverFeatures() {
  return fs
    .readdirSync(FEATURES_DIR, { withFileTypes: true })
    .filter((d) => d.isDirectory() && !d.name.startsWith('.'))
    .map((d) => d.name)
    .sort();
}

function crossFeatureRules(features) {
  const rules = [];
  for (const fromFeature of features) {
    for (const toFeature of features) {
      if (fromFeature === toFeature) continue;
      rules.push({
        name: `no-cross-feature-${fromFeature}-to-${toFeature}`,
        comment: `features/${fromFeature} must not import features/${toFeature}; share via services/* or store/*`,
        severity: 'error',
        from: { path: `^src/features/${fromFeature}/` },
        to: { path: `^src/features/${toFeature}/` },
      });
    }
  }
  return rules;
}

/** @type {import('dependency-cruiser').IConfiguration} */
module.exports = {
  forbidden: [
    ...crossFeatureRules(discoverFeatures()),
    {
      name: 'shared-not-from-features',
      comment: 'shared infra must not depend on a feature',
      severity: 'error',
      from: {
        path: '^src/(components|hooks|services|store|api|utils|types|assets)/',
      },
      to: {
        path: '^src/features/',
      },
    },
  ],
  options: {
    doNotFollow: {
      path: 'node_modules',
    },
    tsConfig: {
      fileName: path.join(__dirname, 'tsconfig.json'),
    },
    enhancedResolveOptions: {
      exportsFields: ['exports'],
      conditionNames: ['import', 'require', 'browser', 'default'],
    },
    includeOnly: '^src/',
  },
};
