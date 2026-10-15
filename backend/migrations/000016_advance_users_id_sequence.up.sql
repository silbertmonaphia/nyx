SELECT setval(
    pg_get_serial_sequence('users', 'id'),
    COALESCE(MAX(id), 1),
    MAX(id) IS NOT NULL
) FROM users;
