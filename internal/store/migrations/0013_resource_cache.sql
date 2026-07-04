ALTER TABLE servers ADD COLUMN cached_cpu_milli  int NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN cached_memory_mb  int NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN cached_storage_mb int NOT NULL DEFAULT 0;
