import { Feed, NewFeed, PaginatedFeeds } from '../types/feed';
import api from '~/services/api'; // Use the alias here

const DEFAULT_PAGE_SIZE = 20;

export const feedService = {
  async getFeeds(
    searchTerm: string = '',
    page: number = 1,
    pageSize: number = DEFAULT_PAGE_SIZE,
  ): Promise<PaginatedFeeds> {
    const params = new URLSearchParams();
    if (searchTerm) params.set('q', searchTerm);
    params.set('page', String(page));
    params.set('page_size', String(pageSize));
    const response = await api.get(`/feeds?${params.toString()}`);
    return response.data;
  },

  async addFeed(feed: NewFeed): Promise<Feed> {
    const response = await api.post('/feeds', feed);
    return response.data;
  },

  async updateFeed(id: number, feed: Partial<Feed>): Promise<Feed> {
    const response = await api.put(`/feeds/${id}`, feed);
    return response.data;
  },

  async deleteFeed(id: number): Promise<void> {
    await api.delete(`/feeds/${id}`);
  },
};