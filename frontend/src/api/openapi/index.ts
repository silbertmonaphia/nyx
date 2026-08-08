// Re-exports of the generated OpenAPI schema types. These aliases exist so
// consumers don't need to know that the codegen nests everything under
// `components["schemas"][...]`. If you need a new wire type, add it here
// alongside the others — do not import directly from `./schema` elsewhere.

import type { components } from './schema';

export type Movie = components['schemas']['Movie'];
export type MoviesPage = components['schemas']['MoviesPage'];
export type MovieInput = components['schemas']['Movie'];
export type User = components['schemas']['User'];
export type AuthResponse = components['schemas']['AuthResponse'];
export type LoginRequest = components['schemas']['LoginRequest'];
export type RegisterRequest = components['schemas']['RegisterRequest'];
export type ErrorResponse = components['schemas']['ErrorResponse'];

// The shape the global axios error interceptor reads from 4xx/5xx bodies.
// `details` is intentionally typed as `unknown` here: the OpenAPI spec
// declares it as an open schema (`{}`), so the generated type is `unknown`,
// and we deliberately do not narrow it in the interceptor — handlers
// surface user-facing detail strings via their own typed responses.
export type ApiError = ErrorResponse;
export type ApiErrorDetail = unknown;