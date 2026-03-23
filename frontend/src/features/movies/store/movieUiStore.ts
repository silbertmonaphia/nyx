import { create } from 'zustand';
import { Movie } from '../types/movie';

interface MovieUiState {
  searchTerm: string;
  showAddForm: boolean;
  editingMovie: Movie | null;
  setSearchTerm: (term: string) => void;
  setShowAddForm: (show: boolean) => void;
  setEditingMovie: (movie: Movie | null) => void;
  resetFormState: () => void;
}

export const useMovieUiStore = create<MovieUiState>((set) => ({
  searchTerm: '',
  showAddForm: false,
  editingMovie: null,
  setSearchTerm: (term) => set({ searchTerm: term }),
  setShowAddForm: (show) => set({ showAddForm: show, editingMovie: null }),
  setEditingMovie: (movie) => set({ editingMovie: movie, showAddForm: false }),
  resetFormState: () => set({ showAddForm: false, editingMovie: null }),
}));
