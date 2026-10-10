import js from '@eslint/js';
import tsPlugin from '@typescript-eslint/eslint-plugin';
import react from 'eslint-plugin-react';
import reactHooks from 'eslint-plugin-react-hooks';

// Flat config — the format ESLint 9 already prefers and the only one the next
// major accepts. The rules mirror the previous .eslintrc.json one-for-one
// (eslint:recommended, @typescript-eslint/recommended, react/recommended,
// react/jsx-runtime, react-hooks/recommended) plus the two explicit errors, so
// the migration changes the config format and nothing else.
export default [
  { ignores: ['dist', 'node_modules'] },
  js.configs.recommended,
  ...tsPlugin.configs['flat/recommended'],
  react.configs.flat.recommended,
  react.configs.flat['jsx-runtime'],
  reactHooks.configs.flat.recommended,
  {
    settings: { react: { version: 'detect' } },
    rules: {
      'no-console': 'error',
      'react-hooks/exhaustive-deps': 'error',

      // react-hooks 7 ships React-Compiler-readiness rules that did not exist
      // in the previous plugin major. This codebase does not run the compiler,
      // and the three rules below flag long-standing, deliberate patterns
      // (seeding form state from fetched data behind a `seeded` guard, ref
      // reads in the data hooks, in-place updates in the live log stream).
      // Kept off so this dependency bump does not silently redefine the lint
      // gate; revisit as its own change if the compiler is ever adopted.
      'react-hooks/set-state-in-effect': 'off',
      'react-hooks/refs': 'off',
      'react-hooks/immutability': 'off',
    },
  },
];
