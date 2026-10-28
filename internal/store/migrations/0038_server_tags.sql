-- Free-form labels on a server for grouping and filtering the server list.
ALTER TABLE servers ADD COLUMN tags text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 6);
