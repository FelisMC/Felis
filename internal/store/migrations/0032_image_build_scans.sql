-- Scan record per build: the Trivy verdict and report and the CycloneDX SBOM (both gzip), read from the build pod's log. The row goes with its build.
CREATE TABLE image_build_scans (
  build_id text PRIMARY KEY REFERENCES image_builds(id) ON DELETE CASCADE,
  blocked boolean NOT NULL,
  summary jsonb NOT NULL,
  report_gz bytea,
  sbom_gz bytea,
  scanned_at timestamptz NOT NULL
);
