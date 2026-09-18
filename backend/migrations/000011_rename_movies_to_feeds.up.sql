-- Rename the movie-catalog table to feed. Pure rename — indexes,
-- constraints, FKs, and serial sequence follow the table automatically.
ALTER TABLE movies RENAME TO feeds;