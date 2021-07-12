DROP FUNCTION IF EXISTS refresh_latest_packages_view;
DROP INDEX IF EXISTS package_latest_cache_pkey;
DROP MATERIALIZED VIEW IF EXISTS package_latest_cache;


CREATE MATERIALIZED VIEW IF NOT EXISTS package_latest_cache
AS
SELECT p.name_id, p.id as package_id, sum.value as summary
    FROM (SELECT DISTINCT ON (package.name_id) package.name_id, package.advisory_id, package.summary_hash, package.id FROM package) as p
    INNER JOIN strings sum on p.summary_hash = sum.id
    LEFT JOIN advisory_metadata am on p.advisory_id = am.id
    ORDER BY p.advisory_id NULLS LAST, p.name_id, am.public_date DESC;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE package_latest_cache TO vmaas_sync;

GRANT SELECT ON TABLE public.package_latest_cache TO evaluator;
GRANT SELECT ON TABLE public.package_latest_cache TO listener;
GRANT SELECT ON TABLE public.package_latest_cache TO manager;

CREATE UNIQUE INDEX IF NOT EXISTS package_latest_cache_pkey ON package_latest_cache (name_id);

REFRESH MATERIALIZED VIEW package_latest_cache;

CREATE OR REPLACE FUNCTION refresh_latest_packages_view()
    RETURNS void
    SECURITY DEFINER
AS $$
BEGIN
    REFRESH MATERIALIZED VIEW package_latest_cache WITH DATA;
    RETURN;
END;
$$ LANGUAGE plpgsql;
