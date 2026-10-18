-- Existing administrators require explicit breakGlass promotion to the owner role.

ALTER TYPE user_role ADD VALUE 'owner';
