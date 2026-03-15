INSERT INTO movies (title, description, rating) VALUES 
('Inception', 'A thief who steals corporate secrets through the use of dream-sharing technology.', 8.8),
('The Matrix', 'A computer hacker learns from mysterious rebels about the true nature of his reality.', 8.7),
('Interstellar', 'A team of explorers travel through a wormhole in space in an attempt to ensure humanity''s survival.', 8.6)
ON CONFLICT DO NOTHING;