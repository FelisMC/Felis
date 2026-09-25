-- The scan record of each build: scan-gate's verdict, the full Trivy JSON
-- report and the CycloneDX SBOM (both gzip), read back from the build pod's log
-- when the Job finishes. A blocked build keeps the findings that blocked it; an
-- admitted image keeps its SBOM, reached from image_whitelist.build_id. The row
-- goes with its build.
CREATE TABLE image_build_scans (
  build_id text PRIMARY KEY REFERENCES image_builds(id) ON DELETE CASCADE,
  blocked boolean NOT NULL,
  summary jsonb NOT NULL,
  report_gz bytea,
  sbom_gz bytea,
  scanned_at timestamptz NOT NULL
);
