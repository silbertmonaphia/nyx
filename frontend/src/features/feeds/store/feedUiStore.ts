import { create } from 'zustand';
import { Feed } from '../types/feed';

interface FeedUiState {
  searchTerm: string;
  showAddForm: boolean;
  editingFeed: Feed | null;
  setSearchTerm: (term: string) => void;
  setShowAddForm: (show: boolean) => void;
  setEditingFeed: (feed: Feed | null) => void;
  resetFormState: () => void;
}

export const useFeedUiStore = create<FeedUiState>((set) => ({
  searchTerm: '',
  showAddForm: false,
  editingFeed: null,
  setSearchTerm: (term) => set({ searchTerm: term }),
  setShowAddForm: (show) => set({ showAddForm: show, editingFeed: null }),
  setEditingFeed: (feed) => set({ editingFeed: feed, showAddForm: false }),
  resetFormState: () => set({ showAddForm: false, editingFeed: null }),
}));