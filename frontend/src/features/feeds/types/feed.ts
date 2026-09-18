import { z } from 'zod';
import type { Feed as ApiFeed, FeedsPage as ApiFeedsPage } from '~/api/openapi';

// Define the validation schema for a feed entry
export const feedSchema = z.object({
  title: z.string().min(1, 'Title is required').max(100, 'Title is too long'),
  description: z.string().max(1000, 'Description is too long').optional().default(''),
  rating: z.coerce.number().min(0, 'Rating must be at least 0').max(10, 'Rating cannot exceed 10'),
});

// Derive TypeScript types from the schema
export type FeedFormData = z.infer<typeof feedSchema>;

export type NewFeed = FeedFormData;

// Wire types — sourced from the generated OpenAPI schema so the frontend
// stays in lockstep with the backend. `Feed` is huma's response shape
// (id + timestamps are server-controlled); `FeedsPage` is the pagination
// envelope returned by GET /api/feeds.
export type Feed = ApiFeed;
export type PaginatedFeeds = ApiFeedsPage;