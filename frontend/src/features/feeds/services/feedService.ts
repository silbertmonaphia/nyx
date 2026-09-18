import { Feed, NewFeed, PaginatedFeeds } from '../types/feed';
import api from '~/services/api'; // Use the alias here
import type { SortOrder } from '../store/feedUiStore';

const DEFAULT_PAGE_SIZE = 5;

export const feedService = {
  async getFeeds(
    searchTerm: string = '',
    page: number = 1,
    pageSize: number = DEFAULT_PAGE_SIZE,
    order: SortOrder = 'desc',
  ): Promise<PaginatedFeeds> {
    const params = new URLSearchParams();
    if (searchTerm) params.set('q', searchTerm);
    params.set('page', String(page));
    params.set('page_size', String(pageSize));
    params.set('order', order);
    const response = await api.get(`/feeds?${params.toString()}`);
    return response.data;
  },

  async addFeed(feed: NewFeed): Promise<Feed> {
    const response = await api.post('/feeds', feed);
    return response.data;
  },

  async updateFeed(id: number, feed: Partial<Feed>): Promise<Feed> {
    // Strip server-controlled fields. Backend FeedInput has
    // additionalProperties:false (huma strict-mode), so an extra
    // id/created_at/updated_at/deleted_at in the PUT body would 400
    // as "unexpected property" — pick only the user-editable subset.
    // Rating is omitted from the form, so we don't send it; the
    // backend treats FeedInput.Rating as optional and leaves the
    // existing value untouched.
    const response = await api.put(`/feeds/${id}`, {
      title: feed.title,
      description: feed.description,
    });
    return response.data;
  },

  async deleteFeed(id: number): Promise<void> {
    await api.delete(`/feeds/${id}`);
  },
};