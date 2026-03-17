export interface Movie {
  id: number;
  title: string;
  description: string;
  rating: number;
  created_at?: string;
  updated_at?: string;
}

export type NewMovie = Omit<Movie, 'id' | 'created_at' | 'updated_at'>;
