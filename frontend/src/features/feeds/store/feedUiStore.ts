import { create } from 'zustand';
import { Feed } from '../types/feed';

// 'desc' matches the backend default (newest first). The toggle
// button flips between desc and asc; 'desc' is the value the
// handler accepts as the default when ?order is absent.
export type SortOrder = 'desc' | 'asc';

interface FeedUiState {
  searchTerm: string;
  showAddForm: boolean;
  editingFeed: Feed | null;
  sortOrder: SortOrder;
  setSearchTerm: (term: string) => void;
  setShowAddForm: (show: boolean) => void;
  setEditingFeed: (feed: Feed | null) => void;
  setSortOrder: (order: SortOrder) => void;
  toggleSortOrder: () => void;
  resetFormState: () => void;
  // Wipe every user-scoped field (search, modals, edit target) but
  // keep `sortOrder` — it's a personal preference, not data. Called
  // when the authenticated user changes so user B never sees user A's
  // leftover UI state.
  reset: () => void;
}

export const useFeedUiStore = create<FeedUiState>((set, get) => ({
  searchTerm: '',
  showAddForm: false,
  editingFeed: null,
  sortOrder: 'desc',
  setSearchTerm: (term) => set({ searchTerm: term }),
  setShowAddForm: (show) => set({ showAddForm: show, editingFeed: null }),
  setEditingFeed: (feed) => set({ editingFeed: feed, showAddForm: false }),
  setSortOrder: (order) => set({ sortOrder: order }),
  toggleSortOrder: () =>
    set({ sortOrder: get().sortOrder === 'desc' ? 'asc' : 'desc' }),
  resetFormState: () => set({ showAddForm: false, editingFeed: null }),
  reset: () => set({ searchTerm: '', showAddForm: false, editingFeed: null }),
}));