ALTER TABLE servers ADD COLUMN protected boolean NOT NULL DEFAULT false;

CREATE TABLE server_collaborators (
  server_name text NOT NULL REFERENCES servers(name) ON DELETE CASCADE,
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role        text NOT NULL CHECK (role IN ('operator', 'manager')),
  granted_by  text REFERENCES users(id) ON DELETE SET NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (server_name, user_id)
);

CREATE INDEX server_collaborators_user_idx ON server_collaborators (user_id);
