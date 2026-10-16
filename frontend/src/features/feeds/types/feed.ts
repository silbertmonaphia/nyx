import { z } from 'zod';
import { t } from '~/i18n';
import type { Feed as ApiFeed, FeedsPage as ApiFeedsPage } from '~/api/openapi';

// Define the validation schema for a feed entry
export const feedSchema = z.object({
  title: z
    .string()
    .min(1, { error: () => t('feeds.titleRequired') })
    .max(100, { error: () => t('feeds.titleTooLong') }),
  description: z
    .string()
    .max(1000, { error: () => t('feeds.descriptionTooLong') })
    .optional()
    .default(''),
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